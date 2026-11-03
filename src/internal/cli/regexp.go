package cli

import (
	"sync/atomic"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// JSON Schema patterns use ECMAScript syntax, including lookahead assertions
// used by the interactive CLI. Match failures invalidate the entire validation,
// including patterns inside negated schemas; the caller schema stays intact.
type schemaRegexp struct {
	*regexp2.Regexp
	failed *atomic.Bool
}

func (r schemaRegexp) MatchString(s string) bool {
	matched, err := r.Regexp.MatchString(s)
	if err != nil {
		r.failed.Store(true)
	}
	return err == nil && matched
}

func (b *bridge) compileRegexp(pattern string) (jsonschema.Regexp, error) {
	r, err := regexp2.Compile(pattern, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	r.MatchTimeout = 50 * time.Millisecond
	return schemaRegexp{r, &b.regexFailed}, nil
}
