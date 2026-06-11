package observe

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// ExtractFields runs the parser stages over a log line and returns the
// extracted fields. Stages apply in order; later stages overwrite duplicate
// keys. Unparseable lines extract nothing (never an error — a mixed stream
// must not fail the query; rows simply gain no fields and label filters drop
// them naturally).
func ExtractFields(line string, stages []ParserStage) map[string]string {
	var out map[string]string
	for _, st := range stages {
		var fields map[string]string
		switch st {
		case ParserLogfmt:
			fields = extractLogfmt(line)
		case ParserJSON:
			fields = extractJSON(line)
		}
		if len(fields) == 0 {
			continue
		}
		if out == nil {
			out = fields
			continue
		}
		for k, v := range fields {
			out[k] = v
		}
	}
	return out
}

// labelKeyRe bounds extracted keys to label-safe identifiers; anything else
// is dropped rather than minting unaddressable labels.
var labelKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// extractLogfmt parses key=value pairs: bare values run to the next space,
// quoted values honour escapes. Tokens without '=' are skipped.
func extractLogfmt(line string) map[string]string {
	out := map[string]string{}
	i := 0
	n := len(line)
	for i < n {
		// skip spaces
		for i < n && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		// key
		start := i
		for i < n && line[i] != '=' && line[i] != ' ' && line[i] != '\t' {
			i++
		}
		if i >= n || line[i] != '=' {
			continue // bare word, no '='
		}
		key := line[start:i]
		i++ // consume '='
		// value
		var val string
		if i < n && line[i] == '"' {
			j := i + 1
			for j < n {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == '"' {
					break
				}
				j++
			}
			if j >= n {
				// unterminated quote: take the rest raw
				val = line[i+1:]
				i = n
			} else {
				if unq, err := strconv.Unquote(line[i : j+1]); err == nil {
					val = unq
				} else {
					val = line[i+1 : j]
				}
				i = j + 1
			}
		} else {
			vstart := i
			for i < n && line[i] != ' ' && line[i] != '\t' {
				i++
			}
			val = line[vstart:i]
		}
		if labelKeyRe.MatchString(key) {
			out[key] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractJSON parses a JSON object line and stringifies its top-level scalar
// fields. Nested objects/arrays are skipped (Advanced-tier territory).
func extractJSON(line string) map[string]string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if !labelKeyRe.MatchString(k) {
			continue
		}
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			out[k] = strconv.FormatBool(t)
		case nil:
			out[k] = ""
		default:
			// objects/arrays skipped
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CompiledLabelFilters pre-compiles label filters for row-rate evaluation.
type CompiledLabelFilters []compiledLabelFilter

type compiledLabelFilter struct {
	label string
	op    LabelFilterOp
	value string
	num   float64
	isNum bool
	re    *regexp.Regexp
}

// CompileLabelFilters validates and compiles the filters (regex compilation,
// numeric threshold parsing).
func CompileLabelFilters(fs []LabelFilter) (CompiledLabelFilters, error) {
	out := make(CompiledLabelFilters, 0, len(fs))
	for _, f := range fs {
		cf := compiledLabelFilter{label: f.Label, op: f.Op, value: f.Value}
		switch f.Op {
		case LabelFilterRe, LabelFilterNotRe:
			re, err := regexp.Compile(f.Value)
			if err != nil {
				return nil, err
			}
			cf.re = re
		case LabelFilterGT, LabelFilterGTE, LabelFilterLT, LabelFilterLTE, LabelFilterNumEq:
			n, err := strconv.ParseFloat(f.Value, 64)
			if err != nil {
				return nil, &labelFilterValueError{f}
			}
			cf.num = n
			cf.isNum = true
		}
		out = append(out, cf)
	}
	return out, nil
}

type labelFilterValueError struct{ f LabelFilter }

func (e *labelFilterValueError) Error() string {
	return "observe: numeric label filter " + e.f.Label + " " + string(e.f.Op) + " " + e.f.Value + ": value is not a number"
}

// Matches evaluates the filters (ANDed) against a label lookup. Numeric
// filters drop rows whose label value is missing or non-numeric.
func (cfs CompiledLabelFilters) Matches(get func(string) (string, bool)) bool {
	for _, cf := range cfs {
		v, ok := get(cf.label)
		var hit bool
		switch cf.op {
		case LabelFilterEq:
			hit = ok && v == cf.value
		case LabelFilterNeq:
			hit = !ok || v != cf.value
		case LabelFilterRe:
			hit = ok && cf.re.MatchString(v)
		case LabelFilterNotRe:
			hit = !ok || !cf.re.MatchString(v)
		default: // numeric
			if !ok {
				return false
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return false
			}
			switch cf.op {
			case LabelFilterGT:
				hit = n > cf.num
			case LabelFilterGTE:
				hit = n >= cf.num
			case LabelFilterLT:
				hit = n < cf.num
			case LabelFilterLTE:
				hit = n <= cf.num
			case LabelFilterNumEq:
				hit = n == cf.num
			}
		}
		if !hit {
			return false
		}
	}
	return true
}
