// Copyright 2020 The klaytn Authors
// This file is part of the klaytn library.
//
// The klaytn library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The klaytn library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the klaytn library. If not, see <http://www.gnu.org/licenses/>.

package kafka

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	"github.com/klaytn/klaytn/common"
	"github.com/stretchr/testify/suite"
)

type KafkaSuite struct {
	suite.Suite
	kfk      *Kafka
	consumer sarama.Consumer
	topic    string
}

// In order to test KafkaSuite, any available kafka broker must be connectable with "kafka:9094".
// If no kafka broker is available, the KafkaSuite tests are skipped.
func (s *KafkaSuite) SetupTest() {
	config := GetDefaultKafkaConfig()
	config.Brokers = []string{"kafka:9094"}
	kfk, err := NewKafka(config)
	if err == sarama.ErrOutOfBrokers {
		s.T().Log("Failed connecting to brokers", config.Brokers)
		s.T().Skip()
	}
	s.NoError(err)
	s.kfk = kfk

	consumer, err := sarama.NewConsumer(config.Brokers, config.SaramaConfig)
	s.NoError(err)
	s.consumer = consumer
	s.topic = "test-topic"
}

func (s *KafkaSuite) TearDownTest() {
	s.kfk.Close()
}

func (s *KafkaSuite) TestKafka_split() {
	segmentSizeBytes := 3
	s.kfk.config.SegmentSizeBytes = segmentSizeBytes

	// test with the size less than the segment size
	bytes := common.MakeRandomBytes(segmentSizeBytes - 1)
	parts, size := s.kfk.split(bytes)
	s.Equal(bytes, parts[0])
	s.Equal(1, size)

	// test with the given segment size
	bytes = common.MakeRandomBytes(segmentSizeBytes)
	parts, size = s.kfk.split(bytes)
	s.Equal(bytes, parts[0])
	s.Equal(1, size)

	// test with the size greater than the segment size
	bytes = common.MakeRandomBytes(2*segmentSizeBytes + 2)
	parts, size = s.kfk.split(bytes)
	s.Equal(bytes[:segmentSizeBytes], parts[0])
	s.Equal(bytes[segmentSizeBytes:2*segmentSizeBytes], parts[1])
	s.Equal(bytes[2*segmentSizeBytes:], parts[2])
	s.Equal(3, size)
}

func (s *KafkaSuite) TestKafka_makeProducerMessage() {
	// make test data
	data := common.MakeRandomBytes(100)
	rand.Seed(time.Now().UnixNano())
	totalSegments := rand.Uint64()
	idx := rand.Uint64() % totalSegments

	// make a producer message with the random input
	msg := s.kfk.makeProducerMessage(s.topic, "", data, idx, totalSegments)

	// compare the data is correctly inserted
	s.Equal(s.topic, msg.Topic)
	s.Equal(sarama.ByteEncoder(data), msg.Value)
	s.Equal(totalSegments, binary.BigEndian.Uint64(msg.Headers[MsgHeaderTotalSegments].Value))
	s.Equal(idx, binary.BigEndian.Uint64(msg.Headers[MsgHeaderSegmentIdx].Value))
}

func (s *KafkaSuite) TestKafka_setupTopic() {
	topicName := "test-setup-topic"

	// create a new topic
	err := s.kfk.setupTopic(topicName)
	s.NoError(err)

	// try to create duplicated topic
	err = s.kfk.setupTopic(topicName)
	s.NoError(err)
}

func (s *KafkaSuite) TestKafka_CreateAndDeleteTopic() {
	// no topic to be deleted
	err := s.kfk.DeleteTopic(s.topic)
	s.Error(err)
	s.True(strings.Contains(err.Error(), sarama.ErrUnknownTopicOrPartition.Error()))

	// created a topic successfully
	err = s.kfk.CreateTopic(s.topic)
	s.NoError(err)

	// failed to create a duplicated topic
	err = s.kfk.CreateTopic(s.topic)
	s.Error(err)
	s.True(strings.Contains(err.Error(), sarama.ErrTopicAlreadyExists.Error()))

	// deleted a topic successfully
	s.Nil(s.kfk.DeleteTopic(s.topic))

	topics, err := s.kfk.ListTopics()
	if _, exist := topics[s.topic]; exist {
		s.Fail("topic must not exist")
	}
}

type kafkaData struct {
	Data []byte `json:"data"`
}

func (s *KafkaSuite) publishRandomData(topic string, numTests, testBytesSize int) []*kafkaData {
	var expected []*kafkaData
	for i := 0; i < numTests; i++ {
		testData := &kafkaData{common.MakeRandomBytes(testBytesSize)}
		s.NoError(s.kfk.Publish(topic, testData))
		expected = append(expected, testData)
	}
	return expected
}

func (s *KafkaSuite) subscribeData(topic, groupId string, numTests int, handler func(message *sarama.ConsumerMessage) error) {
	numCheckCh := make(chan struct{}, numTests)

	// make a test consumer group
	s.kfk.config.SaramaConfig.Consumer.Offsets.Initial = sarama.OffsetOldest
	consumer, err := NewConsumer(s.kfk.config, groupId)
	s.NoError(err)
	defer consumer.Close()

	// add handler for the test event group
	consumer.topics = append(consumer.topics, topic)
	consumer.handlers[topic] = func(message *sarama.ConsumerMessage) error {
		err := handler(message)
		numCheckCh <- struct{}{}
		return err
	}

	// subscribe the added topics
	go func() {
		err := consumer.Subscribe(context.Background())
		s.NoError(err)
	}()

	// wait for all data to be consumed
	timeout := time.NewTimer(5 * time.Second)
	for i := 0; i < numTests; i++ {
		select {
		case <-numCheckCh:
			s.T().Logf("test count: %v, total tests: %v", i+1, numTests)
		case <-timeout.C:
			s.Fail("timeout")
		}
	}
}

func (s *KafkaSuite) TestKafka_Publish() {
	numTests := 10
	testBytesSize := 100

	s.kfk.CreateTopic(s.topic)

	expected := s.publishRandomData(s.topic, numTests, testBytesSize)

	// consume from the first partition and the first item
	partitionConsumer, err := s.consumer.ConsumePartition(s.topic, int32(0), int64(0))
	s.NoError(err)

	var actual []*kafkaData
	i := 0
	for msg := range partitionConsumer.Messages() {
		var dec *kafkaData
		json.Unmarshal(msg.Value, &dec)
		actual = append(actual, dec)
		i++
		if i == numTests {
			break
		}
	}

	s.True(len(actual) == numTests)
	for idx, v := range expected {
		s.Equal(v, actual[idx])
	}
}

func (s *KafkaSuite) TestKafka_Subscribe() {
	numTests := 10
	testBytesSize := 100

	topic := "test-subscribe"
	s.kfk.CreateTopic(topic)

	expected := s.publishRandomData(topic, numTests, testBytesSize)

	var actual []*kafkaData
	s.subscribeData(topic, "test-group-id", numTests, func(message *sarama.ConsumerMessage) error {
		var d *kafkaData
		json.Unmarshal(message.Value, &d)
		actual = append(actual, d)
		return nil
	})

	// compare the results with the published data
	s.Equal(expected, actual)
}

func (s *KafkaSuite) TestKafka_PubSubWith2Partitions() {
	numTests := 10
	testBytesSize := 100

	s.kfk.config.Partitions = 2
	defer func() { s.kfk.config.Partitions = DefaultPartitions }()

	topicPartition := "test-2-partition-topic"
	s.kfk.CreateTopic(topicPartition)

	// publish random data
	expected := s.publishRandomData(topicPartition, numTests, testBytesSize)

	var actual []*kafkaData
	s.subscribeData(topicPartition, "test-group-id", numTests, func(message *sarama.ConsumerMessage) error {
		var d *kafkaData
		json.Unmarshal(message.Value, &d)
		actual = append(actual, d)
		return nil
	})

	// the number of partitions is not 1, so the order may be changed after subscription.
	// compare the results with the published data
	s.Equal(len(expected), len(actual))

	for _, e := range expected {
		has := false
		for _, a := range actual {
			if reflect.DeepEqual(e, a) {
				has = true
			}
		}
		if !has {
			s.Fail("the expected data is not contained in the actual data", "expected", e)
		}
	}
}

func (s *KafkaSuite) TestKafka_PubSubWith2DifferentGroups() {
	numTests := 10
	testBytesSize := 100

	topic := "test-different-groups"
	s.kfk.CreateTopic(topic)

	// publish random data
	expected := s.publishRandomData(topic, numTests, testBytesSize)

	var actual []*kafkaData
	s.subscribeData(topic, "test-group-id-1", numTests, func(message *sarama.ConsumerMessage) error {
		var d *kafkaData
		json.Unmarshal(message.Value, &d)
		actual = append(actual, d)
		return nil
	})

	var actual2 []*kafkaData
	s.subscribeData(topic, "test-group-id-2", numTests, func(message *sarama.ConsumerMessage) error {
		var d *kafkaData
		json.Unmarshal(message.Value, &d)
		actual2 = append(actual2, d)
		return nil
	})

	// the number of partitions is not 1, so the order may be changed after subscription.
	// compare the results with the published data
	s.Equal(expected, actual)
	s.Equal(expected, actual2)
}

func (s *KafkaSuite) TestKafka_PubSubWithSegments() {
	numTests := 5
	testBytesSize := 10
	segmentSize := 3

	s.kfk.config.SegmentSizeBytes = segmentSize
	topic := "test-message-segments"
	s.kfk.CreateTopic(topic)

	// publish random data
	expected := s.publishRandomData(topic, numTests, testBytesSize)

	var actual []*kafkaData
	s.subscribeData(topic, "test-group-id", numTests, func(message *sarama.ConsumerMessage) error {
		var d *kafkaData
		json.Unmarshal(message.Value, &d)
		actual = append(actual, d)
		return nil
	})
	s.Equal(expected, actual)
}

func (s *KafkaSuite) TestKafka_PubSubWithSegements_BufferOverflow() {
	// create a topic
	topic := "test-message-segments-buffer-overflow"
	err := s.kfk.setupTopic(topic)
	s.NoError(err)

	// insert incomplete message segments
	for i := 0; i < 3; i++ {
		msg := s.kfk.makeProducerMessage(topic, "test-key-"+strconv.Itoa(i), common.MakeRandomBytes(10), 0, 2)
		_, _, err = s.kfk.producer.SendMessage(msg)
		s.NoError(err)
	}

	// setup consumer to handle errors
	s.kfk.config.MaxMessageNumber = 1 // if buffer size is greater than 1, then it returns an error
	s.kfk.config.SaramaConfig.Consumer.Return.Errors = true
	s.kfk.config.SaramaConfig.Consumer.Offsets.Initial = sarama.OffsetOldest
	consumer, err := NewConsumer(s.kfk.config, "test-group-id")
	s.NoError(err)
	consumer.topics = append(consumer.topics, topic)
	consumer.handlers[topic] = func(message *sarama.ConsumerMessage) error { return nil }
	errCh := consumer.Errors()

	go func() {
		err = consumer.Subscribe(context.Background())
		s.NoError(err)
	}()

	// checkout the returned error is buffer overflow error
	timeout := time.NewTimer(3 * time.Second)
	select {
	case <-timeout.C:
		s.Fail("timeout")
	case err := <-errCh:
		s.True(strings.Contains(err.Error(), bufferOverflowErrorMsg))
	}
}

func (s *KafkaSuite) TestKafka_Consumer_AddTopicAndHandler() {
	consumer, err := NewConsumer(s.kfk.config, "test-group-id")
	s.NoError(err)

	blockGroupHandler := func(msg *sarama.ConsumerMessage) error { return nil }
	s.NoError(consumer.AddTopicAndHandler(EventBlockGroup, blockGroupHandler))
	traceGroupHandler := func(msg *sarama.ConsumerMessage) error { return nil }
	s.NoError(consumer.AddTopicAndHandler(EventTraceGroup, traceGroupHandler))

	blockGroupTopic := s.kfk.config.GetTopicName(EventBlockGroup)
	traceGroupTopic := s.kfk.config.GetTopicName(EventTraceGroup)
	expectedTopics := []string{blockGroupTopic, traceGroupTopic}
	s.Equal(expectedTopics, consumer.topics)
	s.Equal(reflect.ValueOf(blockGroupHandler).Pointer(), reflect.ValueOf(consumer.handlers[blockGroupTopic]).Pointer())
	s.Equal(reflect.ValueOf(traceGroupHandler).Pointer(), reflect.ValueOf(consumer.handlers[traceGroupTopic]).Pointer())
}

func (s *KafkaSuite) TestKafka_Consumer_AddTopicAndHandler_Error() {
	consumer, err := NewConsumer(s.kfk.config, "test-group-id")
	s.NoError(err)

	err = consumer.AddTopicAndHandler("not-available-event", nil)
	s.Error(err)
	s.True(strings.Contains(err.Error(), eventNameErrorMsg))
}

func TestKafkaSuite(t *testing.T) {
	suite.Run(t, new(KafkaSuite))
}
