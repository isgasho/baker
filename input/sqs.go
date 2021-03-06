package input

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/AdRoll/baker"
	"github.com/AdRoll/baker/input/inpututils"
	"github.com/AdRoll/baker/pkg/awsutils"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
)

var SQSDesc = baker.InputDesc{
	Name:   "SQS",
	New:    NewSQS,
	Config: &SQSConfig{},
	Help: "This input listens on multiple SQS queues for new incoming log files\n" +
		"on S3; it is meant to be used with SQS queues popoulated by SNS.\n" +
		"It never exits.\n",
}

const (
	sqsFormatPlain = "plain"
	sqsFormatSNS   = "sns"
)

type SQSConfig struct {
	AwsRegion      string   `help:"AWS region to connect to" default:"us-west-2"`
	Bucket         string   `help:"S3 Bucket to use for processing" default:""`
	QueuePrefixes  []string `help:"Prefixes of the names of the SQS queues to monitor" required:"true"`
	MessageFormat  string   `help:"The format of the SQS messages.\n'plain' the SQS messages received have the S3 file path as a plain string.\n'sns' the SQS messages were produced by a SNS notification." default:"sns"`
	FilePathFilter string   `help:"If provided, will only use S3 files with the given path."`
}

func (cfg *SQSConfig) fillDefaults() {
	if cfg.AwsRegion == "" {
		cfg.AwsRegion = "us-west-2"
	}
	if cfg.MessageFormat == "" {
		cfg.MessageFormat = sqsFormatSNS
	} else {
		cfg.MessageFormat = strings.ToLower(cfg.MessageFormat)
	}
}

type SQS struct {
	s3Input *inpututils.S3Input

	Cfg            *SQSConfig
	FilePathRegexp *regexp.Regexp
	svc            *sqs.SQS
	wg             sync.WaitGroup
	done           chan bool

	minSnsTimestamp time.Time
}

func NewSQS(cfg baker.InputParams) (baker.Input, error) {
	if cfg.DecodedConfig == nil {
		cfg.DecodedConfig = &SQSConfig{}
	}
	dcfg := cfg.DecodedConfig.(*SQSConfig)
	dcfg.fillDefaults()

	sess := session.New(&aws.Config{Region: aws.String(dcfg.AwsRegion)})
	svc := sqs.New(sess)

	var filePathRegexp *regexp.Regexp
	if dcfg.FilePathFilter != "" {
		var err error
		filePathRegexp, err = regexp.Compile(dcfg.FilePathFilter)
		if err != nil {
			return nil, err
		}
	} else {
		filePathRegexp = nil
	}

	return &SQS{
		s3Input:         inpututils.NewS3Input(dcfg.AwsRegion, dcfg.Bucket),
		Cfg:             dcfg,
		svc:             svc,
		FilePathRegexp:  filePathRegexp,
		minSnsTimestamp: time.Time{},
		done:            make(chan bool),
	}, nil
}

// pollQueue polls the given queue as long as the given context is alive.
func (s *SQS) pollQueue(ctx context.Context, sqsurl string) {
	ctxLog := log.WithFields(log.Fields{"f": "SQS.pollQueue", "url": sqsurl})
	backoff := awsutils.DefaultBackoff
	for {
		resp, err := s.svc.ReceiveMessageWithContext(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:        aws.String(sqsurl),
			WaitTimeSeconds: aws.Int64(20),
			// We ask only for 1 message at a time, because the
			// parseFile() call below could block, and we want to
			// receive messages and not process them immediately,
			// or they could get rescheduled to other readers.
			MaxNumberOfMessages: aws.Int64(1),
		})
		if ctx.Err() == context.Canceled || ctx.Err() == context.DeadlineExceeded {
			return
		}

		if err != nil {
			ctxLog.WithError(err).Error("error from ReceiveMessage")
			time.Sleep(backoff.Duration())
			continue
		}
		backoff.Reset()

		for _, msg := range resp.Messages {
			var s3FilePath string
			var snsMsgTimestamp string

			s3FilePath, snsMsgTimestamp, err := s.parseMessage(msg.Body, ctxLog)
			if err != nil {
				continue
			}

			if snsMsgTimestamp != "" {
				// Track the minimum timestamp of the SNS
				// notification. Stats() will reset it once a second, so
				// in practice we track the minimum ts seen in each
				// second.
				ts, err := time.Parse(time.RFC3339, snsMsgTimestamp)
				if err != nil {
					ctxLog.WithError(err).Error("error parsing Timestamp in SNS message")
					continue
				}

				if s.minSnsTimestamp.IsZero() || ts.Unix() < s.minSnsTimestamp.Unix() {
					s.minSnsTimestamp = ts
				}
			}

			// Skip the file if it doesn't match the filter provided.
			if s.FilePathRegexp == nil || s.FilePathRegexp.MatchString(s3FilePath) {
				// FIXME: we should check if the bucket matches what was configured
				// or even better, change s3Input to not be limited to a single bucket
				s.s3Input.ParseFile(s3FilePath)
			}

			_, err = s.svc.DeleteMessageWithContext(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(sqsurl),
				ReceiptHandle: msg.ReceiptHandle,
			})
			if ctx.Err() == context.Canceled || ctx.Err() == context.DeadlineExceeded {
				return
			}
			if err != nil {
				ctxLog.WithError(err).Error("error from DeleteMessage")
			}
		}
	}
}

func (s *SQS) parseMessage(Body *string, ctxLog *log.Entry) (string, string, error) {
	var s3FilePath string
	var snsMsgTimestamp string

	switch s.Cfg.MessageFormat {
	case sqsFormatPlain:
		// The SQS queue is populated by a lambda function that
		// just provides the path to the S3 file in the message's
		// body.
		s3FilePath = string(*Body)
		snsMsgTimestamp = ""

	case sqsFormatSNS:
		// The SQS queue is populated by SNS messages. So the
		// body is a JSON document with several fields; we only
		// care about one field: "Message", which is the URL of
		// the file on S3 that was generated.
		type SNSMessage struct {
			Message   string
			Timestamp string // time SNS notification was received by SNS
		}
		snsMsg := SNSMessage{}
		if err := json.Unmarshal([]byte(*Body), &snsMsg); err != nil {
			ctxLog.WithError(err).Error("error parsing SNS message in SQS")
			return "", "", err
		}

		// The URL sent through SNS is something like:
		//   s3n://BUCKET/path
		// So we just extract the path and use it as filename
		parsedUrl, err := url.Parse(snsMsg.Message)
		if err != nil {
			ctxLog.WithError(err).Error("error parsing URL in SNS message in SQS")
			return "", "", err
		}
		// If bucket isn't hardcoded, find it from S3 path.
		if s.Cfg.Bucket == "" {
			s3FilePath = snsMsg.Message
		} else {
			s3FilePath = parsedUrl.Path[1:]
		}
		snsMsgTimestamp = snsMsg.Timestamp
	}
	return s3FilePath, snsMsgTimestamp, nil
}

func (s *SQS) Run(inch chan<- *baker.Data) error {
	s.s3Input.SetOutputChannel(inch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for _, prefix := range s.Cfg.QueuePrefixes {

		resp, err := s.svc.ListQueuesWithContext(ctx, &sqs.ListQueuesInput{
			QueueNamePrefix: aws.String(prefix),
		})
		if err != nil {
			return err
		}

		for _, url := range resp.QueueUrls {
			wg.Add(1)
			go func(url string) {
				defer wg.Done()

				s.pollQueue(ctx, url)
			}(*url)
		}
	}

	// The correct order of operation to cleanly stop the whole pipeline is the
	// following:
	//  - first we close the 'done' channel, this in turns cancel polling via
	//    context cancellation
	//  - we then wait for all the polling goroutines to end.
	//  - After this point we are guaranteed to not receive any more files so
	//    we notify the embedded S3input.
	//  - now we ask S3Input to stop as soon as it has finished processing files
	//  - finally Run exits after being signaled from S3Input that we can
	<-s.done
	cancel()
	wg.Wait()
	s.s3Input.NoMoreFiles()
	s.s3Input.Stop()
	<-s.s3Input.Done
	return nil
}

func (s *SQS) Stop() {
	// See the comment at the end of the Run method for details about
	// how this brings the whole topology down cleanly.
	close(s.done)
}

func (s *SQS) Stats() baker.InputStats {
	bag := make(baker.MetricsBag)

	if !s.minSnsTimestamp.IsZero() {
		bag.AddGauge("sqs.lag", time.Since(s.minSnsTimestamp).Seconds())

		// Reset on each poll, which in practice means we'll get the
		// minimum of each second.
		s.minSnsTimestamp = time.Time{}
	}

	stats := s.s3Input.Stats()
	stats.Metrics = bag
	return stats
}

func (s *SQS) FreeMem(data *baker.Data) {
	s.s3Input.FreeMem(data)
}
