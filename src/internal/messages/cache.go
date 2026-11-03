package messages

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
)

// NormalizeCache consumes ephemeral caching hints without importing client cache
// boundaries into a different native prompt. One uniform TTL can be applied by
// the official CLI. Mixed TTLs cannot be represented and are rejected.
func (r Request) NormalizeCache() (Request, error) {
	consume := func(cache *CacheControl) error {
		if cache == nil {
			return nil
		}
		if err := validateCache(cache); err != nil {
			return err
		}
		ttl := cache.TTL
		if ttl == "" {
			ttl = "5m"
		}
		if r.CacheTTL != "" && r.CacheTTL != ttl {
			return unsupported("cache_control: mixed TTLs require client cache boundaries")
		}
		r.CacheTTL = ttl
		return nil
	}
	var content func(jsontext.Value) (jsontext.Value, error)
	content = func(raw jsontext.Value) (jsontext.Value, error) {
		if len(raw) == 0 || raw.Kind() == '"' {
			return raw, nil
		}
		blocks, err := Blocks(raw)
		if err != nil {
			return nil, err
		}
		for i, block := range blocks {
			var fields map[string]jsontext.Value
			if err := json.Unmarshal(block, &fields); err != nil {
				return nil, err
			}
			if value := fields["cache_control"]; len(value) > 0 {
				delete(fields, "cache_control")
				if value.Kind() != 'n' {
					var cache CacheControl
					if err := json.Unmarshal(value, &cache); err != nil {
						return nil, err
					}
					if err := consume(&cache); err != nil {
						return nil, err
					}
				}
			}
			if string(fields["type"]) == `"tool_result"` {
				if value := fields["content"]; len(value) > 0 {
					fields["content"], err = content(value)
					if err != nil {
						return nil, err
					}
				}
			}
			blocks[i], err = json.Marshal(fields)
			if err != nil {
				return nil, err
			}
		}
		return json.Marshal(blocks)
	}
	var err error
	r.System, err = content(r.System)
	if err != nil {
		return Request{}, err
	}
	r.Tools = slices.Clone(r.Tools)
	for i := range r.Tools {
		if err := consume(r.Tools[i].CacheControl); err != nil {
			return Request{}, err
		}
		r.Tools[i].CacheControl = nil
	}
	r.Messages = slices.Clone(r.Messages)
	for i := range r.Messages {
		if err := consume(r.Messages[i].CacheControl); err != nil {
			return Request{}, err
		}
		r.Messages[i].CacheControl = nil
		r.Messages[i].Content, err = content(r.Messages[i].Content)
		if err != nil {
			return Request{}, err
		}
	}
	return r, nil
}
