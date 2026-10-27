package otel

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
)

var (
	chURLUserinfo   = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/?#\s]*@`)
	chPercentEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
)

type chSafeError struct {
	text     string
	original error
	causes   []error
}

func (e *chSafeError) Error() string        { return e.text }
func (e *chSafeError) Unwrap() []error      { return e.causes }
func (e *chSafeError) Is(target error) bool { return errors.Is(e.original, target) }

func redactCHError(err error, endpoint *url.URL) error {
	if err == nil {
		return nil
	}
	var tokens []string
	if endpoint != nil && endpoint.User != nil {
		password, _ := endpoint.User.Password()
		for _, value := range []string{endpoint.User.String(), endpoint.User.Username(), password} {
			if value != "" {
				tokens = append(tokens, value, url.QueryEscape(value), url.PathEscape(value))
			}
		}
	}
	sort.Slice(tokens, func(i, j int) bool { return len(tokens[i]) > len(tokens[j]) })
	patterns := make([]*regexp.Regexp, 0, len(tokens))
	for _, token := range tokens {
		pattern := chPercentEscape.ReplaceAllStringFunc(regexp.QuoteMeta(token), func(escape string) string { return "(?i:" + escape + ")" })
		if compiled, err := regexp.Compile(pattern); err == nil {
			patterns = append(patterns, compiled)
		}
	}
	redact := func(text string) string {
		text = chURLUserinfo.ReplaceAllString(text, "${1}[redacted]@")
		for _, pattern := range patterns {
			text = pattern.ReplaceAllLiteralString(text, "[redacted]")
		}
		for _, token := range tokens {
			text = strings.ReplaceAll(text, token, "[redacted]")
		}
		return text
	}
	return sanitizeCHError(err, redact)
}

func sanitizeCHError(err error, redact func(string) string) error {
	if err == nil {
		return nil
	}
	var otel *OtelError
	switch e := err.(type) {
	case *OtelError:
		otel = e
	case *ConfigError:
		otel = e.OtelError
	case *OwnershipError:
		otel = e.OtelError
	case *AdmissionError:
		otel = e.OtelError
	case *QueryBudgetError:
		otel = e.OtelError
	case *CorruptionError:
		otel = e.OtelError
	case *HTTPError:
		otel = e.OtelError
	case *AuditError:
		otel = e.OtelError
	case *OracleError:
		otel = e.OtelError
	case *OperationError:
		otel = e.OtelError
	case *url.Error:
		clean := *e
		if address, parseErr := url.Parse(e.URL); parseErr == nil {
			address.User = nil
			clean.URL = address.String()
		}
		clean.URL, clean.Op = redact(clean.URL), redact(clean.Op)
		clean.Err = sanitizeCHError(e.Err, redact)
		return &clean
	case *clickhouse.Exception:
		clean := sanitizeCHException(*e, redact)
		return &clean
	}
	if otel != nil {
		return typedError(otel.Kind, redact(otel.Op), redact(otel.Message), sanitizeCHError(otel.Cause, redact))
	}
	var causes []error
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range wrapped.Unwrap() {
			causes = append(causes, sanitizeCHError(cause, redact))
		}
	case interface{ Unwrap() error }:
		causes = append(causes, sanitizeCHError(wrapped.Unwrap(), redact))
	}
	return &chSafeError{text: redact(err.Error()), original: err, causes: causes}
}

func sanitizeCHException(err clickhouse.Exception, redact func(string) string) clickhouse.Exception {
	err.Name, err.Message, err.StackTrace = redact(err.Name), redact(err.Message), redact(err.StackTrace)
	nested := make([]clickhouse.Exception, len(err.Nested))
	for i, child := range err.Nested {
		nested[i] = sanitizeCHException(child, redact)
	}
	err.Nested = nested
	return err
}
