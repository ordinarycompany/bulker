package bulkerlib

import (
	"fmt"
	"github.com/jitsucom/bulker/jitsubase/utils"
)

type StreamOption func(*StreamOptions)

var optionsRegistry = make(map[string]ParseableOption)

var (
	BatchSizeOption = ImplementationOption[int]{
		Key:          "batchSize",
		DefaultValue: 0,
		ParseFunc:    utils.ParseInt,
	}

	// BatchFrequencyOption frequency of running batches in minutes
	BatchFrequencyOption = ImplementationOption[float64]{
		Key:          "frequency",
		DefaultValue: 0,
		ParseFunc:    utils.ParseFloat,
	}

	RetryBatchSizeOption = ImplementationOption[int]{
		Key:          "retryBatchSize",
		DefaultValue: 0,
		ParseFunc:    utils.ParseInt,
	}
	// RetryFrequencyOption frequency of running retry consumer in minutes
	RetryFrequencyOption = ImplementationOption[float64]{
		Key:          "retryFrequency",
		DefaultValue: 0,
		ParseFunc:    utils.ParseFloat,
	}

	ModeOption = ImplementationOption[BulkMode]{Key: "mode", ParseFunc: func(serialized any) (BulkMode, error) {
		switch v := serialized.(type) {
		case string:
			if v == "stream" {
				return Stream, nil
			} else if v == "batch" {
				return Batch, nil
			} else {
				return Unknown, fmt.Errorf("unknown mode: %s", v)
			}
		default:
			return Unknown, fmt.Errorf("invalid value type of mode option: %T", v)
		}
	},
	}

	PrimaryKeyOption = ImplementationOption[utils.Set[string]]{
		Key:          "primaryKey",
		DefaultValue: utils.Set[string]{},
		AdvancedParseFunc: func(o *ImplementationOption[utils.Set[string]], serializedValue any) (StreamOption, error) {
			switch v := serializedValue.(type) {
			case []string:
				return withPrimaryKey(o, v...), nil
			case string:
				if v == "" {
					return func(options *StreamOptions) {}, nil
				}
				return withPrimaryKey(o, v), nil
			default:
				return nil, fmt.Errorf("failed to parse 'primaryKey' option: %v incorrect type: %T expected string or []string", v, v)
			}
		},
	}

	MergeRowsOption = ImplementationOption[bool]{
		Key:          "deduplicate",
		DefaultValue: false,
		ParseFunc:    utils.ParseBool,
	}

	PartitionIdOption = ImplementationOption[string]{
		Key:       "partitionId",
		ParseFunc: utils.ParseString,
	}

	// TimestampOption - field name that contains timestamp. For creating sorting indexes or partitions by that field in destination tables
	TimestampOption = ImplementationOption[string]{
		Key:       "timestampColumn",
		ParseFunc: utils.ParseString,
	}

	// Not used by bulker. Just added here to be treated as known options
	FunctionsOption  = ImplementationOption[any]{Key: "functions", ParseFunc: func(serialized any) (any, error) { return nil, nil }}
	StreamsOption    = ImplementationOption[any]{Key: "streams", ParseFunc: func(serialized any) (any, error) { return nil, nil }}
	DataLayoutOption = ImplementationOption[string]{Key: "dataLayout", ParseFunc: utils.ParseString}
	EventsOption     = ImplementationOption[string]{Key: "events", ParseFunc: utils.ParseString}
	HostsOption      = ImplementationOption[string]{Key: "hosts", ParseFunc: utils.ParseString}
)

func init() {
	RegisterOption(&ModeOption)
	RegisterOption(&BatchSizeOption)
	RegisterOption(&BatchFrequencyOption)
	RegisterOption(&RetryFrequencyOption)
	RegisterOption(&RetryBatchSizeOption)
	RegisterOption(&PrimaryKeyOption)
	RegisterOption(&MergeRowsOption)
	RegisterOption(&PartitionIdOption)
	RegisterOption(&TimestampOption)

	// Not used by bulker. Just added here to be treated as known options
	RegisterOption(&FunctionsOption)
	RegisterOption(&StreamsOption)
	RegisterOption(&DataLayoutOption)
	RegisterOption(&EventsOption)
	RegisterOption(&HostsOption)

}

func RegisterOption[V any](option *ImplementationOption[V]) {
	optionsRegistry[option.Key] = option
}

func ParseOption(name string, serialized any) (StreamOption, error) {
	option, ok := optionsRegistry[name]
	if !ok {
		return nil, fmt.Errorf("unknown option %s", name)
	}
	return option.Parse(serialized)

}

type StreamOptions struct {
	// Implementation options - map by option key. Values are parsed and validated
	// Don't access this map directly, use 'Get' method of specific option instance. E.g. `PartitionIdOption.Get(&so)`
	valuesMap map[string]any
	// options slice. To pass to CreateStream method
	Options []StreamOption
}

func (so *StreamOptions) Add(option StreamOption) {
	option(so)
	so.Options = append(so.Options, option)
}

type ParseableOption interface {
	Parse(serialized any) (StreamOption, error)
}

type ImplementationOption[V any] struct {
	Key               string
	DefaultValue      V
	ParseFunc         func(serialized any) (V, error)
	AdvancedParseFunc func(*ImplementationOption[V], any) (StreamOption, error)
}

func (io *ImplementationOption[V]) Parse(serializedValue any) (StreamOption, error) {
	if io.ParseFunc != nil {
		val, err := io.ParseFunc(serializedValue)
		if err != nil {
			return nil, fmt.Errorf("failed to parse '%s' option: %v", io.Key, err)
		}
		return func(options *StreamOptions) {
			io.Set(options, val)
		}, nil
	} else {
		return io.AdvancedParseFunc(io, serializedValue)
	}
}

func (io *ImplementationOption[V]) Get(so *StreamOptions) V {
	opt, ok := so.valuesMap[io.Key].(V)
	if ok {
		return opt
	}
	return io.DefaultValue
}

func (io *ImplementationOption[V]) Set(so *StreamOptions, value V) {
	if so.valuesMap == nil {
		so.valuesMap = map[string]any{io.Key: value}
	} else {
		so.valuesMap[io.Key] = value
	}
}

func withPrimaryKey(o *ImplementationOption[utils.Set[string]], pkFields ...string) StreamOption {
	return func(options *StreamOptions) {
		set := o.Get(options)
		if len(set) == 0 {
			o.Set(options, utils.NewSet(pkFields...))
		} else {
			set.PutAll(pkFields)
		}
	}
}

func WithPrimaryKey(pkFields ...string) StreamOption {
	return withPrimaryKey(&PrimaryKeyOption, pkFields...)
}

// WithMergeRows - when true merge rows on primary keys collision.
func withMergeRows(o *ImplementationOption[bool], b bool) StreamOption {
	return func(options *StreamOptions) {
		o.Set(options, b)
	}
}

// WithMergeRows - when true merge rows on primary keys collision.
func WithMergeRows() StreamOption {
	return withMergeRows(&MergeRowsOption, true)
}

func WithoutMergeRows() StreamOption {
	return withMergeRows(&MergeRowsOption, false)

}

func withPartition(o *ImplementationOption[string], partitionId string) StreamOption {
	return func(options *StreamOptions) {
		o.Set(options, partitionId)
	}
}

// WithPartition settings for bulker.ReplacePartition mode only
// partitionId - value of `__partition_id`  for current BulkerStream e.g. id of current partition
// TODO: For bigquery require string in special format
func WithPartition(partitionId string) StreamOption {
	return withPartition(&PartitionIdOption, partitionId)
}

func withTimestamp(o *ImplementationOption[string], timestampField string) StreamOption {
	return func(options *StreamOptions) {
		o.Set(options, timestampField)
	}
}

func WithTimestamp(timestampField string) StreamOption {
	return withTimestamp(&TimestampOption, timestampField)
}
