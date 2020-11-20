---
title: "ReplaceFields"
weight: 13
date: 2020-11-12
---
## Filter *ReplaceFields*

### Overview
Copy a field value or a fixed value to another field.
 Can copy multiple fields.


### Configuration

Keys available in the `[filter.config]` section:

|Name|Type|Default|Required|Description|
|:--:|:--:|:-----:|:------:|:---------:|
| CopyFields| array of strings| | false| List of src, dst field pairs, for example ["srcField1", "dstField1", "srcField2", "dstField2"]|
| ReplaceFields| array of strings| | false| List of field, value pairs, for example: ["Foo", "dstField1", "Bar", "dstField2"]|
