// gen-gitleaks-rules turns the upstream gitleaks rule pack into an OTel
// transform-processor config fragment that the Dagger Collector loads as
// collector/redaction.yaml.
//
// The output is committed so reviewers can read the expanded regex set in
// diffs; the generator is run by hand when bumping gitleaksVersion below
// (there is no go:generate hook on purpose — the rule pack changes slowly
// and a silent regen on every build would mask drift).
//
// Run from the repository root:
//
//	go run ./collector/gen-gitleaks-rules \
//	    -out collector/redaction.yaml
//
// Pass -input to use a local gitleaks.toml instead of fetching it.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// gitleaksVersion is the upstream rule pack this generator was last validated
// against. Bump it, re-run the generator and review the diff in
// collector/redaction.yaml.
const gitleaksVersion = "v8.30.1"

func gitleaksURL() string {
	return fmt.Sprintf(
		"https://raw.githubusercontent.com/gitleaks/gitleaks/%s/config/gitleaks.toml",
		gitleaksVersion)
}

type rule struct {
	id    string
	regex string
}

func main() {
	in := flag.String("input", "", "path to gitleaks.toml; if empty the pinned upstream copy is fetched")
	out := flag.String("out", "collector/redaction.yaml", "output path for the OTel transform-processor fragment")
	flag.Parse()

	var src io.ReadCloser
	if *in != "" {
		f, err := os.Open(*in)
		if err != nil {
			fail("open %s: %v", *in, err)
		}
		src = f
	} else {
		r, err := fetch(gitleaksURL())
		if err != nil {
			fail("fetch %s: %v", gitleaksURL(), err)
		}
		src = r
	}
	defer func() { _ = src.Close() }()

	rules, err := parseRules(src)
	if err != nil {
		fail("parse: %v", err)
	}

	kept, dropped := compilable(rules)
	sort.Slice(kept, func(i, j int) bool { return kept[i].id < kept[j].id })

	if err := writeConfig(*out, kept); err != nil {
		fail("write %s: %v", *out, err)
	}

	fmt.Fprintf(os.Stderr, "gitleaks %s: %d rules kept, %d dropped (no regex or RE2-incompatible)\n",
		gitleaksVersion, len(kept), dropped)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gen-gitleaks-rules: "+format+"\n", args...)
	os.Exit(1)
}

func fetch(url string) (io.ReadCloser, error) {
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// parseRules pulls (id, regex) pairs out of a gitleaks.toml without taking on
// a TOML dependency. Every rule in the pinned upstream pack is shaped
//
//	[[rules]]
//	id = "..."
//	regex = '''...'''
//	... (other fields ignored)
//
// with single-line triple-quoted regex literals. Multi-line regexes would
// silently parse as no-regex rules and be dropped by compilable; we'd notice
// in the dropped count, then teach the parser.
func parseRules(r io.Reader) ([]rule, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var rules []rule
	var cur rule
	inRule := false

	flush := func() {
		if inRule && cur.id != "" {
			rules = append(rules, cur)
		}
		cur = rule{}
		inRule = false
	}

	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "[[rules]]":
			flush()
			inRule = true
		case strings.HasPrefix(trimmed, "[") && trimmed != "[[rules]]":
			// Entering a non-rule table; the current rule is done.
			flush()
		case inRule && strings.HasPrefix(trimmed, "id = "):
			cur.id = strings.Trim(strings.TrimPrefix(trimmed, "id = "), `"`)
		case inRule && strings.HasPrefix(trimmed, "regex = '''") && strings.HasSuffix(trimmed, "'''"):
			body := strings.TrimPrefix(trimmed, "regex = '''")
			body = strings.TrimSuffix(body, "'''")
			cur.regex = body
		}
	}
	flush()

	if err := sc.Err(); err != nil {
		return nil, err
	}
	return rules, nil
}

// compilable filters out rules with no regex (e.g. file-extension-only rules
// like pkcs12-file) and rules whose regex Go's RE2 engine rejects, since
// the OTel transform processor uses the same engine — an uncompilable rule
// would crash the Collector on startup, not silently no-op.
func compilable(rules []rule) (kept []rule, dropped int) {
	for _, r := range rules {
		if r.regex == "" {
			dropped++
			continue
		}
		if _, err := regexp.Compile(r.regex); err != nil {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	return kept, dropped
}

// writeConfig emits redaction.yaml. The fragment defines a single
// `transform/redaction` processor whose statements walk log bodies, log
// attributes and span attributes, substituting any match for
// "REDACTED:<rule-id>". collector/config.yaml lists this processor in its
// log/trace pipelines; the two files are merged at Collector startup via
// multiple --config flags.
func writeConfig(path string, rules []rule) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# AUTO-GENERATED by collector/gen-gitleaks-rules from\n")
	fmt.Fprintf(&b, "# gitleaks %s (%s). DO NOT EDIT — re-run the generator and review\n",
		gitleaksVersion, gitleaksURL())
	fmt.Fprintf(&b, "# the diff. Loaded alongside collector/config.yaml; see README.\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# %d rules emitted as OTTL replace_pattern / replace_all_patterns\n", len(rules))
	fmt.Fprintf(&b, "# statements; the matched substring is overwritten with\n")
	fmt.Fprintf(&b, "# REDACTED:<rule-id> so post-redaction telemetry still tells you which\n")
	fmt.Fprintf(&b, "# class of secret was scrubbed.\n\n")

	b.WriteString("processors:\n")
	b.WriteString("  transform/redaction:\n")
	b.WriteString("    error_mode: ignore\n")
	b.WriteString("    log_statements:\n")
	b.WriteString("      - context: log\n")
	b.WriteString("        statements:\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "          - %s\n",
			yamlSingle(fmt.Sprintf(
				`replace_pattern(body, %s, %s) where IsString(body)`,
				ottlString(r.regex), ottlString("REDACTED:"+r.id))))
	}
	for _, r := range rules {
		fmt.Fprintf(&b, "          - %s\n",
			yamlSingle(fmt.Sprintf(
				`replace_all_patterns(attributes, "value", %s, %s)`,
				ottlString(r.regex), ottlString("REDACTED:"+r.id))))
	}

	b.WriteString("    trace_statements:\n")
	b.WriteString("      - context: span\n")
	b.WriteString("        statements:\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "          - %s\n",
			yamlSingle(fmt.Sprintf(
				`replace_all_patterns(attributes, "value", %s, %s)`,
				ottlString(r.regex), ottlString("REDACTED:"+r.id))))
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ottlString quotes s as an OTTL double-quoted string literal. Escapes
// match the OTTL grammar (\\, \", \n, \r, \t) so the embedded regex parses
// back byte-for-byte.
func ottlString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// yamlSingle wraps an arbitrary string for use as a single-quoted YAML
// scalar. Single-quoted YAML only recognises one escape — a doubled
// apostrophe stands for a literal one — which keeps the backslash-heavy
// OTTL payload from being re-processed by YAML.
func yamlSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
