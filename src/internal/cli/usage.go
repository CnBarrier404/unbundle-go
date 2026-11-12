package cli

import "unbundle-go/src/internal/messages"

func usageFields(u *messages.Usage) []**int64 {
	return []**int64{&u.InputTokens, &u.OutputTokens, &u.CacheReadInputTokens, &u.CacheCreationInputTokens}
}

func zeroUsage() (u messages.Usage) {
	for _, field := range usageFields(&u) {
		value := int64(0)
		*field = &value
	}
	return u
}

func addUsage(a, b messages.Usage) (u messages.Usage) {
	x, y := usageFields(&a), usageFields(&b)
	for i, field := range usageFields(&u) {
		if *x[i] != nil && *y[i] != nil {
			value := **x[i] + **y[i]
			if value >= 0 {
				*field = &value
			}
		}
	}
	return u
}

func usageDifference(total, accounted messages.Usage) (u messages.Usage, err error) {
	x, y := usageFields(&total), usageFields(&accounted)
	for i, field := range usageFields(&u) {
		if *x[i] != nil && **x[i] < 0 || *y[i] != nil && **y[i] < 0 {
			return u, protocol()
		}
		if *x[i] != nil && *y[i] != nil {
			value := **x[i] - **y[i]
			if value < 0 {
				return u, protocol()
			}
			*field = &value
		}
	}
	return u, nil
}

func mergeUsage(dst *messages.Usage, src messages.Usage) error {
	target, source := usageFields(dst), usageFields(&src)
	for i, field := range source {
		if *field != nil && (**field < 0 || *target[i] != nil && **field < **target[i]) {
			return protocol()
		}
	}
	for i, field := range source {
		if *field != nil {
			*target[i] = *field
		}
	}
	return nil
}

type modelUsage struct {
	Input  *int64 `json:"inputTokens"`
	Output *int64 `json:"outputTokens"`
	Read   *int64 `json:"cacheReadInputTokens"`
	Write  *int64 `json:"cacheCreationInputTokens"`
}

func modelUsageDifference(before, after map[string]modelUsage) (messages.Usage, error) {
	if len(before) == 0 || len(after) == 0 {
		return messages.Usage{}, Failure("COMPACTION_USAGE_UNKNOWN", "compaction", "native compaction usage could not be verified; do not repeat the operation", 502)
	}
	total := zeroUsage()
	for name := range before {
		if _, ok := after[name]; !ok {
			return messages.Usage{}, protocol()
		}
	}
	for name, next := range after {
		previous, exists := before[name]
		a := next.messagesUsage()
		b := zeroUsage()
		if exists {
			b = previous.messagesUsage()
		}
		delta, err := usageDifference(a, b)
		if err != nil {
			return messages.Usage{}, err
		}
		total = addUsage(total, delta)
	}
	return total, nil
}

func (u modelUsage) messagesUsage() messages.Usage {
	return messages.Usage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadInputTokens: u.Read, CacheCreationInputTokens: u.Write}
}
