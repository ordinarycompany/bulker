package app

import (
	"bytes"
	"context"
	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/jitsucom/bulker/bulkerapp/metrics"
	bulker "github.com/jitsucom/bulker/bulkerlib"
	"github.com/jitsucom/bulker/bulkerlib/types"
	"github.com/jitsucom/bulker/jitsubase/appbase"
	"github.com/jitsucom/bulker/jitsubase/safego"
	"github.com/jitsucom/bulker/jitsubase/timestamp"
	"github.com/jitsucom/bulker/jitsubase/utils"
	jsoniter "github.com/json-iterator/go"
	"strconv"
	"sync/atomic"
	"time"
)

const streamConsumerMessageWaitTimeout = 1 * time.Second

type StreamConsumer struct {
	appbase.Service
	config         *Config
	repository     *Repository
	destination    *Destination
	stream         atomic.Pointer[bulker.BulkerStream]
	consumerConfig kafka.ConfigMap
	consumer       *kafka.Consumer

	bulkerProducer   *Producer
	eventsLogService EventsLogService

	topicId   string
	tableName string

	closed chan struct{}
}

func NewStreamConsumer(repository *Repository, destination *Destination, topicId string, config *Config, kafkaConfig *kafka.ConfigMap, bulkerProducer *Producer, eventsLogService EventsLogService) (*StreamConsumer, error) {
	base := appbase.NewServiceBase(topicId)
	_, _, tableName, err := ParseTopicId(topicId)
	if err != nil {
		metrics.ConsumerErrors(topicId, "stream", "INVALID_TOPIC", "INVALID_TOPIC:"+topicId, "failed to parse topic").Inc()
		return nil, base.NewError("Failed to parse topic: %v", err)
	}
	consumerConfig := kafka.ConfigMap(utils.MapPutAll(kafka.ConfigMap{
		"group.id":                      topicId,
		"auto.offset.reset":             "earliest",
		"group.instance.id":             config.InstanceId,
		"partition.assignment.strategy": config.KafkaConsumerPartitionsAssigmentStrategy,
		"enable.auto.commit":            true,
		"isolation.level":               "read_committed",
	}, *kafkaConfig))

	consumer, err := kafka.NewConsumer(&consumerConfig)
	if err != nil {
		metrics.ConsumerErrors(topicId, "stream", destination.Id(), tableName, metrics.KafkaErrorCode(err)).Inc()
		return nil, base.NewError("Error creating kafka consumer: %v", err)
	}

	err = consumer.SubscribeTopics([]string{topicId}, nil)
	if err != nil {
		_ = consumer.Close()
		metrics.ConsumerErrors(topicId, "stream", destination.Id(), tableName, metrics.KafkaErrorCode(err)).Inc()
		return nil, base.NewError("Failed to subscribe to topic: %v", err)
	}

	//destination := repository.LeaseDestination(destinationId)
	//if destination == nil {
	//	return nil, fmt.Errorf("[%s] Destination not found", destinationId)
	//}

	sc := &StreamConsumer{
		Service:          base,
		config:           config,
		repository:       repository,
		destination:      destination,
		topicId:          topicId,
		tableName:        tableName,
		consumerConfig:   consumerConfig,
		consumer:         consumer,
		bulkerProducer:   bulkerProducer,
		eventsLogService: eventsLogService,
		closed:           make(chan struct{}),
	}
	bulkerStream, err := sc.destination.bulker.CreateStream(sc.topicId, sc.tableName, bulker.Stream, sc.destination.streamOptions.Options...)
	if err != nil {
		metrics.ConsumerErrors(sc.topicId, "stream", destination.Id(), tableName, "failed to create bulker stream").Inc()
		return nil, base.NewError("Failed to create bulker stream: %v", err)
	}
	sc.stream.Store(&bulkerStream)
	sc.start()
	sc.destination.Lease()
	return sc, nil
}

func (sc *StreamConsumer) restartConsumer() {
	sc.Infof("Restarting consumer")
	go func(c *kafka.Consumer) {
		err := c.Close()
		sc.Infof("Previous consumer closed: %v", err)
	}(sc.consumer)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-sc.closed:
			return
		case <-ticker.C:
			sc.Infof("Restarting consumer")
			consumer, err := kafka.NewConsumer(&sc.consumerConfig)
			if err != nil {
				metrics.ConsumerErrors(sc.topicId, "stream", sc.destination.Id(), sc.tableName, metrics.KafkaErrorCode(err)).Inc()
				sc.Errorf("Error creating kafka consumer: %v", err)
				break
			}
			err = consumer.SubscribeTopics([]string{sc.topicId}, nil)
			if err != nil {
				metrics.ConsumerErrors(sc.topicId, "stream", sc.destination.Id(), sc.tableName, metrics.KafkaErrorCode(err)).Inc()
				_ = consumer.Close()
				sc.Errorf("Failed to subscribe to topic: %v", err)
				break
			}
			sc.consumer = consumer
			sc.Infof("Restarted successfully")
			return
		}
	}
}

// start consuming messages from kafka
func (sc *StreamConsumer) start() {
	sc.Infof("Starting stream consumer for topic. Ver: %s", sc.destination.config.UpdatedAt)
	safego.RunWithRestart(func() {
		var err error
		for {
			select {
			case <-sc.closed:
				_ = sc.consumer.Close()
				var state bulker.State
				if err != nil {
					state, _ = (*sc.stream.Load()).Abort(context.Background())
				} else {
					state, _ = (*sc.stream.Load()).Complete(context.Background())
				}
				sc.Infof("Closed stream state: %+v", state)
				return
			default:
				var message *kafka.Message
				message, err = sc.consumer.ReadMessage(streamConsumerMessageWaitTimeout)
				if err != nil {
					kafkaErr := err.(kafka.Error)
					if kafkaErr.Code() != kafka.ErrTimedOut {
						metrics.ConsumerErrors(sc.topicId, "stream", sc.destination.Id(), sc.tableName, metrics.KafkaErrorCode(kafkaErr)).Inc()
						sc.Errorf("Error reading message from topic: %v retriable: %t", kafkaErr, kafkaErr.IsRetriable())
						if kafkaErr.IsRetriable() {
							time.Sleep(streamConsumerMessageWaitTimeout * 10)
						} else {
							sc.restartConsumer()
						}
					}
					continue
				}
				metrics.ConsumerMessages(sc.topicId, "stream", sc.destination.Id(), sc.tableName, "consumed").Inc()
				obj := types.Object{}
				dec := jsoniter.NewDecoder(bytes.NewReader(message.Value))
				dec.UseNumber()
				err = dec.Decode(&obj)
				if err != nil {
					metrics.ConsumerErrors(sc.topicId, "stream", sc.destination.Id(), sc.tableName, "parse_event_error").Inc()
					sc.postEventsLog(message.Value, nil, nil, err)
					sc.Errorf("Failed to parse event from message: %s offset: %s: %v", message.Value, message.TopicPartition.Offset.String(), err)
				} else {
					sc.Debugf("Consumed Message ID: %s Offset: %s (Retries: %s) for: %s", obj.Id(), message.TopicPartition.Offset.String(), GetKafkaHeader(message, retriesCountHeader), sc.destination.config.BulkerType)
					var state bulker.State
					var processedObjects []types.Object
					state, processedObjects, err = (*sc.stream.Load()).Consume(context.Background(), obj)
					sc.postEventsLog(message.Value, state.Representation, processedObjects, err)
					if err != nil {
						metrics.ConsumerErrors(sc.topicId, "stream", sc.destination.Id(), sc.tableName, "bulker_stream_error").Inc()
						sc.Errorf("Failed to inject event to bulker stream: %v", err)
					} else {
						metrics.ConsumerMessages(sc.topicId, "stream", sc.destination.Id(), sc.tableName, "processed").Inc()
					}
				}
				if err != nil {
					failedTopic, _ := MakeTopicId(sc.destination.Id(), retryTopicMode, allTablesToken, false)
					retries, err := GetKafkaIntHeader(message, retriesCountHeader)
					if err != nil {
						sc.Errorf("failed to read retry header: %v", err)
					}
					status := "retryScheduled"
					if retries >= sc.config.MessagesRetryCount {
						//no attempts left - send to dead-letter topic
						status = "deadLettered"
						failedTopic, _ = MakeTopicId(sc.destination.Id(), deadTopicMode, allTablesToken, false)
					}
					retryMessage := kafka.Message{
						Key:            message.Key,
						TopicPartition: kafka.TopicPartition{Topic: &failedTopic, Partition: kafka.PartitionAny},
						Headers: []kafka.Header{
							{Key: retriesCountHeader, Value: []byte(strconv.Itoa(retries))},
							{Key: originalTopicHeader, Value: []byte(sc.topicId)},
							{Key: retryTimeHeader, Value: []byte(timestamp.ToISOFormat(RetryBackOffTime(sc.config, retries+1).UTC()))}},
						Value: message.Value,
					}
					err = sc.bulkerProducer.ProduceSync(failedTopic, retryMessage)
					if err != nil {
						sc.Errorf("failed to store event to 'failed' topic: %s: %v", failedTopic, err)
						metrics.ConsumerMessages(sc.topicId, "stream", sc.destination.Id(), sc.tableName, "LOST").Inc()
						continue
					}
					metrics.ConsumerMessages(sc.topicId, "stream", sc.destination.Id(), sc.tableName, "failed").Inc()
					metrics.ConsumerMessages(sc.topicId, "stream", sc.destination.Id(), sc.tableName, status).Inc()
				}

			}
		}
	})
}

// Close consumer
func (sc *StreamConsumer) Close() error {
	sc.Infof("Closing stream consumer. Ver: %s", sc.destination.config.UpdatedAt)
	close(sc.closed)
	sc.destination.Release()
	//TODO: wait for closing?
	return nil
}

// UpdateDestination
func (sc *StreamConsumer) UpdateDestination(destination *Destination) error {
	sc.Infof("[Updating stream consumer for topic. Ver: %s", sc.destination.config.UpdatedAt)
	destination.Lease()

	//create new stream
	bulkerStream, err := destination.bulker.CreateStream(sc.topicId, sc.tableName, bulker.Stream, destination.streamOptions.Options...)
	if err != nil {
		return sc.NewError("Failed to create bulker stream: %v", err)
	}
	oldBulkerStream := sc.stream.Swap(&bulkerStream)
	state, err := (*oldBulkerStream).Complete(context.Background())
	sc.Infof("Previous stream state: %+v", state)
	oldDestination := sc.destination
	oldDestination.Release()

	sc.destination = destination
	return nil
}

func (sc *StreamConsumer) postEventsLog(message []byte, representation any, processedObjects []types.Object, processedErr error) {
	object := map[string]any{
		"original": string(message),
		"status":   "SUCCESS",
	}
	if representation != nil {
		object["representation"] = representation
	}
	if len(processedObjects) > 0 {
		object["mappedData"] = processedObjects
	}

	if processedErr != nil {
		object["error"] = processedErr.Error()
		object["status"] = "FAILED"
		_, err := sc.eventsLogService.PostEvent(EventTypeProcessedError, sc.destination.Id(), object)
		if err != nil {
			sc.Errorf("Failed to post event to events log service: %v", err)
		}
	}
	_, err := sc.eventsLogService.PostEvent(EventTypeProcessedAll, sc.destination.Id(), object)
	if err != nil {
		sc.Errorf("Failed to post event to events log service: %v", err)
	}
}
