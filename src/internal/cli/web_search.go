package cli

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"net/url"

	"unbundle-go/src/internal/messages"
)

// Count only results bound to an observed native WebSearch call. A tool return
// alone is not proof of a search: absent CLI metadata remains unknown.
func observeWebSearch(e *messages.WebSearchEvidence, raw jsontext.Value) {
	e.Calls++
	var data struct {
		SearchCount *int64            `json:"searchCount"`
		Results     *[]jsontext.Value `json:"results"`
	}
	if json.Unmarshal(raw, &data) != nil {
		e.Requests, e.Hits = nil, nil
		return
	}
	if e.Requests != nil {
		if data.SearchCount == nil || *data.SearchCount < 0 || *data.SearchCount > math.MaxInt64-*e.Requests {
			e.Requests = nil
		} else {
			*e.Requests += *data.SearchCount
		}
	}
	if data.Results == nil {
		e.Hits = nil
		return
	}
	for _, result := range *data.Results {
		if result.Kind() == '"' {
			continue
		} // CLI commentary, not a search hit
		var group struct {
			Content *[]struct {
				URL string `json:"url"`
			} `json:"content"`
		}
		if json.Unmarshal(result, &group) != nil || group.Content == nil {
			e.Hits = nil
			return
		}
		for _, hit := range *group.Content {
			parsed, err := url.Parse(hit.URL)
			if err == nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "http") && e.Hits != nil {
				*e.Hits++
			}
		}
	}
}
