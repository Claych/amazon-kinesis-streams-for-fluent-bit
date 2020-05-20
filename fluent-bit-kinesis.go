// Copyright 2019-2020 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//  http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"C"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"github.com/aws/amazon-kinesis-firehose-for-fluent-bit/plugins"
	"github.com/aws/amazon-kinesis-streams-for-fluent-bit/kinesis"
	kinesisAPI "github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/fluent/fluent-bit-go/output"
	"github.com/sirupsen/logrus"
)
import jsoniter "github.com/json-iterator/go"

const (
	// Kinesis API Limit https://docs.aws.amazon.com/sdk-for-go/api/service/kinesis/#Kinesis.PutRecords
	maximumRecordsPerPut = 500
)

const (
	retries = 2
)

var (
	pluginInstances []*kinesis.OutputPlugin
)

func addPluginInstance(ctx unsafe.Pointer) error {
	pluginID := len(pluginInstances)
	output.FLBPluginSetContext(ctx, pluginID)
	instance, err := newKinesisOutput(ctx, pluginID)
	if err != nil {
		return err
	}

	pluginInstances = append(pluginInstances, instance)
	return nil
}

func getPluginInstance(ctx unsafe.Pointer) *kinesis.OutputPlugin {
	pluginID := output.FLBPluginGetContext(ctx).(int)
	return pluginInstances[pluginID]
}

func newKinesisOutput(ctx unsafe.Pointer, pluginID int) (*kinesis.OutputPlugin, error) {
	stream := output.FLBPluginConfigKey(ctx, "stream")
	logrus.Infof("[kinesis %d] plugin parameter stream = '%s'", pluginID, stream)
	region := output.FLBPluginConfigKey(ctx, "region")
	logrus.Infof("[kinesis %d] plugin parameter region = '%s'", pluginID, region)
	dataKeys := output.FLBPluginConfigKey(ctx, "data_keys")
	logrus.Infof("[kinesis %d] plugin parameter data_keys = '%s'", pluginID, dataKeys)
	partitionKey := output.FLBPluginConfigKey(ctx, "partition_key")
	logrus.Infof("[kinesis %d] plugin parameter partition_key = '%s'", pluginID, partitionKey)
	roleARN := output.FLBPluginConfigKey(ctx, "role_arn")
	logrus.Infof("[kinesis %d] plugin parameter role_arn = '%s'", pluginID, roleARN)
	endpoint := output.FLBPluginConfigKey(ctx, "endpoint")
	logrus.Infof("[kinesis %d] plugin parameter endpoint = '%s'", pluginID, endpoint)
	appendNewline := output.FLBPluginConfigKey(ctx, "append_newline")
	logrus.Infof("[kinesis %d] plugin parameter append_newline = %s", pluginID, appendNewline)
	timeKey := output.FLBPluginConfigKey(ctx, "time_key")
	logrus.Infof("[firehose %d] plugin parameter time_key = '%s'\n", pluginID, timeKey)
	timeKeyFmt := output.FLBPluginConfigKey(ctx, "time_key_format")
	logrus.Infof("[firehose %d] plugin parameter time_key_format = '%s'\n", pluginID, timeKeyFmt)

	if stream == "" || region == "" {
		return nil, fmt.Errorf("[kinesis %d] stream and region are required configuration parameters", pluginID)
	}

	if partitionKey == "log" {
		return nil, fmt.Errorf("[kinesis %d] 'log' cannot be set as the partition key", pluginID)
	}

	if partitionKey == "" {
		logrus.Infof("[kinesis %d] no partition key provided. A random one will be generated.", pluginID)
	}

	appendNL := false
	if strings.ToLower(appendNewline) == "true" {
		appendNL = true
	}
	return kinesis.NewOutputPlugin(region, stream, dataKeys, partitionKey, roleARN, endpoint, timeKey, timeKeyFmt, appendNL, pluginID)
}

// The "export" comments have syntactic meaning
// This is how the compiler knows a function should be callable from the C code

//export FLBPluginRegister
func FLBPluginRegister(ctx unsafe.Pointer) int {
	return output.FLBPluginRegister(ctx, "kinesis", "Amazon Kinesis Data Streams Fluent Bit Plugin.")
}

//export FLBPluginInit
func FLBPluginInit(ctx unsafe.Pointer) int {
	plugins.SetupLogger()
	err := addPluginInstance(ctx)
	if err != nil {
		logrus.Errorf("[kinesis] Failed to initialize plugin: %v\n", err)
		return output.FLB_ERROR
	}
	return output.FLB_OK
}

//export FLBPluginFlushCtx
func FLBPluginFlushCtx(ctx, data unsafe.Pointer, length C.int, tag *C.char) int {
	events, timestamps, count := unpackRecords(data, length)
	go flushWithRetries(ctx, tag, count, events, timestamps, retries)
	return output.FLB_OK
}

func flushWithRetries(ctx unsafe.Pointer, tag *C.char, count int, events []map[interface{}]interface{}, timestamps []time.Time, retries int) {
	for i := 0; i < retries; i++ {
		retCode := pluginConcurrentFlush(ctx, tag, count, events, timestamps)
		if retCode != output.FLB_RETRY {
			break
		}
	}
}

func unpackRecords(data unsafe.Pointer, length C.int) (records []map[interface{}]interface{}, timestamps []time.Time, count int) {
	var ret int
	var ts interface{}
	var timestamp time.Time
	var record map[interface{}]interface{}
	count = 0
	all_good := true

	records = make([]map[interface{}]interface{}, 100)
	timestamps = make([]time.Time, 100)

	// Create Fluent Bit decoder
	dec := output.NewDecoder(data, int(length))

	for {
		//Extract Record
		ret, ts, record = output.GetRecord(dec)
		if ret != 0 {
			break
		}

		switch tts := ts.(type) {
		case output.FLBTime:
			timestamp = tts.Time
		case uint64:
			// when ts is of type uint64 it appears to
			// be the amount of seconds since unix epoch.
			timestamp = time.Unix(int64(tts), 0)
		default:
			timestamp = time.Now()
		}

		if record == nil {
			logrus.Info("unpack: null record")
			all_good = false
		} else {
			var json = jsoniter.ConfigCompatibleWithStandardLibrary
			data, err := json.Marshal(record)
			if err == nil {
				if len(data) == 0 {
					logrus.Info("unpack: record has zero length")
					all_good = false
				}
			} else {
				logrus.Info("unpack: unmarshal error")
				all_good = false
			}
		}

		records = append(records, record)
		timestamps = append(timestamps, timestamp)

		count++
	}
	logrus.Infof("Processed %d records", count)
	if all_good {
		logrus.Info("All good")
	} else {
		logrus.Info("Not all good")
	}

	for i := 0; i < count; i++ {
		record = records[i]
		if record == nil {
			logrus.Infof("unpack: %d is null\n", i)
		}
		var json = jsoniter.ConfigCompatibleWithStandardLibrary
		data, err := json.Marshal(record)
		if err == nil {
			logrus.Infof("unpack: %s\n", string(data))
		} else {
			logrus.Info("unpack 2: unmarshal error")
		}
	}

	return records, timestamps, count
}

func pluginConcurrentFlush(ctx unsafe.Pointer, tag *C.char, count int, events []map[interface{}]interface{}, timestamps []time.Time) int {
	var timestamp time.Time
	var event map[interface{}]interface{}

	kinesisOutput := getPluginInstance(ctx)
	fluentTag := C.GoString(tag)
	logrus.Debugf("[kinesis %d] Found logs with tag: %s\n", kinesisOutput.PluginID, fluentTag)

	// Each flush must have its own output buffe r, since flushes can be concurrent
	records := make([]*kinesisAPI.PutRecordsRequestEntry, 0, maximumRecordsPerPut)

	for i := 0; i < count; i++ {
		event = events[i]
		if event == nil {
			logrus.Infof("flush: %d is null\n", i)
			continue
		}
		var json = jsoniter.ConfigCompatibleWithStandardLibrary
		data, err := json.Marshal(event)
		if err == nil {
			logrus.Infof("flush: %s\n", string(data))
		} else {
			logrus.Info("flush: unmarshal error")
		}
	}

	for i := 0; i < count; i++ {
		event = events[i]
		timestamp = timestamps[i]
		retCode := kinesisOutput.AddRecord(&records, event, &timestamp)
		if retCode != output.FLB_OK {
			return retCode
		}
		i++
	}
	retCode := kinesisOutput.Flush(&records)
	if retCode != output.FLB_OK {
		return retCode
	}
	logrus.Debugf("[kinesis %d] Processed %d events with tag %s\n", kinesisOutput.PluginID, count, fluentTag)

	return output.FLB_OK
}

//export FLBPluginExit
func FLBPluginExit() int {
	// Before final exit, call Flush() for all the instances of the Output Plugin
	// for i := range pluginInstances {
	// 	pluginInstances[i].Flush(records)
	// }

	return output.FLB_OK
}

func main() {
}
