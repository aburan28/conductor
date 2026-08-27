// Package policy makes repository-owned routing policy executable.
//
// policies.yaml and dispatch.yaml describe *when* a rule applies with small boolean
// expressions such as `task.estimated_files <= 2 && !task.security_sensitive`. This package
// parses those expressions once, evaluates them against deterministic facts derived from the
// ledger (scopes, attempt history, budget position), and explains every decision it makes.
//
// The language is deliberately tiny: literals, dotted identifiers, comparison, boolean
// logic, list membership, and glob matching. There are no side effects, no loops, and no way
// to call out — a policy can read facts and produce a verdict, nothing else. That keeps it
// safe to evaluate on every claim and safe to accept from a repository.
package policy

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Env supplies the facts an expression can reference. Unknown names evaluate to nil, which
// compares unequal to everything and is falsy — an unset fact never accidentally matches.
type Env interface {
	Lookup(name string) (any, bool)
}

// MapEnv is the simplest Env: a flat map keyed by dotted names.
type MapEnv map[string]any

func (m MapEnv) Lookup(name string) (any, bool) {
	v, ok := m[name]
	return v, ok
}

// Expr is a compiled expression.
type Expr struct {
	src  string
	root node
	vars []string
}

// Source returns the text the expression was compiled from.
func (e *Expr) Source() string { return e.src }

// Vars lists every identifier the expression references, sorted, so a linter can flag
// names no fact provider defines.
func (e *Expr) Vars() []string { return append([]string(nil), e.vars...) }

// Compile parses an expression. An empty expression compiles to one that is always true,
// which is what a rule with no `when` means.
func Compile(src string) (*Expr, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return &Expr{src: src, root: litNode{val: true}}, nil
	}
	p := &parser{toks: lex(src), src: src}
	if p.err != nil {
		return nil, p.err
	}
	root := p.parseOr()
	if p.err != nil {
		return nil, p.err
	}
	if p.peek().kind != tokEOF {
		return nil, p.errorf("unexpected %q", p.peek().text)
	}
	e := &Expr{src: src, root: root}
	seen := map[string]bool{}
	collectVars(root, seen)
	for name := range seen {
		e.vars = append(e.vars, name)
	}
	sort.Strings(e.vars)
	return e, nil
}

// MustCompile is Compile for constant expressions in code.
func MustCompile(src string) *Expr {
	e, err := Compile(src)
	if err != nil {
		panic(err)
	}
	return e
}

// Eval evaluates the expression and returns the resulting value.
func (e *Expr) Eval(env Env) (any, error) {
	if e == nil {
		return true, nil
	}
	return e.root.eval(env)
}

// Bool evaluates the expression as a condition.
func (e *Expr) Bool(env Env) (bool, error) {
	v, err := e.Eval(env)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type tokKind int

const (
	tokEOF tokKind = iota
	tokNumber
	tokString
	tokIdent
	tokOp
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokComma
)

type token struct {
	kind tokKind
	text string
	num  float64
	pos  int
}

func lex(src string) []token {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{kind: tokLParen, text: "(", pos: i})
			i++
		case c == ')':
			toks = append(toks, token{kind: tokRParen, text: ")", pos: i})
			i++
		case c == '[':
			toks = append(toks, token{kind: tokLBracket, text: "[", pos: i})
			i++
		case c == ']':
			toks = append(toks, token{kind: tokRBracket, text: "]", pos: i})
			i++
		case c == ',':
			toks = append(toks, token{kind: tokComma, text: ",", pos: i})
			i++
		case c == '"' || c == '\'':
			quote := c
			j := i + 1
			var b strings.Builder
			closed := false
			for j < len(src) {
				if src[j] == '\\' && j+1 < len(src) {
					b.WriteByte(src[j+1])
					j += 2
					continue
				}
				if src[j] == quote {
					closed = true
					break
				}
				b.WriteByte(src[j])
				j++
			}
			if !closed {
				toks = append(toks, token{kind: tokOp, text: "<unterminated string>", pos: i})
				return append(toks, token{kind: tokEOF, pos: len(src)})
			}
			toks = append(toks, token{kind: tokString, text: b.String(), pos: i})
			i = j + 1
		case c >= '0' && c <= '9' || (c == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == '_') {
				j++
			}
			text := strings.ReplaceAll(src[i:j], "_", "")
			n, err := strconv.ParseFloat(text, 64)
			if err != nil {
				toks = append(toks, token{kind: tokOp, text: "<bad number " + src[i:j] + ">", pos: i})
				return append(toks, token{kind: tokEOF, pos: len(src)})
			}
			toks = append(toks, token{kind: tokNumber, text: src[i:j], num: n, pos: i})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(src) && (isIdentStart(src[j]) || src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			word := src[i:j]
			// Word operators read more naturally than symbols for the list and pattern
			// forms: `task.labels has "docs"`, `task.paths any "docs/**"`.
			switch word {
			case "and":
				toks = append(toks, token{kind: tokOp, text: "&&", pos: i})
			case "or":
				toks = append(toks, token{kind: tokOp, text: "||", pos: i})
			case "not":
				toks = append(toks, token{kind: tokOp, text: "!", pos: i})
			case "has", "in", "matches", "any", "all", "contains", "startswith", "endswith":
				toks = append(toks, token{kind: tokOp, text: word, pos: i})
			default:
				toks = append(toks, token{kind: tokIdent, text: word, pos: i})
			}
			i = j
		default:
			// Symbolic operators, longest first.
			matched := false
			for _, op := range []string{"&&", "||", "==", "!=", "<=", ">=", "<", ">", "!", "+", "-", "*", "/", "%"} {
				if strings.HasPrefix(src[i:], op) {
					toks = append(toks, token{kind: tokOp, text: op, pos: i})
					i += len(op)
					matched = true
					break
				}
			}
			if !matched {
				toks = append(toks, token{kind: tokOp, text: "<bad char " + string(c) + ">", pos: i})
				return append(toks, token{kind: tokEOF, pos: len(src)})
			}
		}
	}
	toks = append(toks, token{kind: tokEOF, pos: len(src)})
	return toks
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ---------------------------------------------------------------------------
// Parser (precedence climbing)
// ---------------------------------------------------------------------------

type parser struct {
	toks []token
	pos  int
	src  string
	err  error
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tokEOF {
		p.pos++
	}
	return t
}

func (p *parser) errorf(format string, args ...any) error {
	if p.err == nil {
		t := p.peek()
		p.err = fmt.Errorf("policy expression %q: at offset %d: %s", p.src, t.pos, fmt.Sprintf(format, args...))
	}
	return p.err
}

func (p *parser) accept(text string) bool {
	if t := p.peek(); t.kind == tokOp && t.text == text {
		p.next()
		return true
	}
	return false
}

func (p *parser) parseOr() node {
	left := p.parseAnd()
	for p.accept("||") {
		right := p.parseAnd()
		left = binNode{op: "||", left: left, right: right}
	}
	return left
}

func (p *parser) parseAnd() node {
	left := p.parseNot()
	for p.accept("&&") {
		right := p.parseNot()
		left = binNode{op: "&&", left: left, right: right}
	}
	return left
}

func (p *parser) parseNot() node {
	if p.accept("!") {
		return notNode{inner: p.parseNot()}
	}
	return p.parseCompare()
}

var compareOps = map[string]bool{
	"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true,
	"has": true, "in": true, "matches": true, "any": true, "all": true,
	"contains": true, "startswith": true, "endswith": true,
}

func (p *parser) parseCompare() node {
	left := p.parseAdd()
	if t := p.peek(); t.kind == tokOp && compareOps[t.text] {
		p.next()
		right := p.parseAdd()
		return binNode{op: t.text, left: left, right: right}
	}
	return left
}

func (p *parser) parseAdd() node {
	left := p.parseMul()
	for {
		t := p.peek()
		if t.kind == tokOp && (t.text == "+" || t.text == "-") {
			p.next()
			right := p.parseMul()
			left = binNode{op: t.text, left: left, right: right}
			continue
		}
		return left
	}
}

func (p *parser) parseMul() node {
	left := p.parseUnary()
	for {
		t := p.peek()
		if t.kind == tokOp && (t.text == "*" || t.text == "/" || t.text == "%") {
			p.next()
			right := p.parseUnary()
			left = binNode{op: t.text, left: left, right: right}
			continue
		}
		return left
	}
}

func (p *parser) parseUnary() node {
	if p.accept("-") {
		return negNode{inner: p.parseUnary()}
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() node {
	t := p.next()
	switch t.kind {
	case tokNumber:
		return litNode{val: t.num}
	case tokString:
		return litNode{val: t.text}
	case tokIdent:
		switch t.text {
		case "true":
			return litNode{val: true}
		case "false":
			return litNode{val: false}
		case "null", "nil":
			return litNode{val: nil}
		}
		// Function call: name(args…)
		if p.peek().kind == tokLParen {
			p.next()
			var args []node
			if p.peek().kind != tokRParen {
				for {
					args = append(args, p.parseOr())
					if p.peek().kind == tokComma {
						p.next()
						continue
					}
					break
				}
			}
			if p.next().kind != tokRParen {
				p.errorf("expected ) after arguments to %s", t.text)
			}
			if !knownFunc(t.text) {
				p.errorf("unknown function %q", t.text)
			}
			return callNode{name: t.text, args: args}
		}
		return varNode{name: t.text}
	case tokLParen:
		inner := p.parseOr()
		if p.next().kind != tokRParen {
			p.errorf("expected )")
		}
		return inner
	case tokLBracket:
		var items []node
		for p.peek().kind != tokRBracket {
			items = append(items, p.parseOr())
			if p.peek().kind == tokComma {
				p.next()
				continue
			}
			if p.peek().kind != tokRBracket {
				p.errorf("expected , or ] in list")
				break
			}
		}
		if p.next().kind != tokRBracket {
			p.errorf("expected ]")
		}
		return listNode{items: items}
	case tokEOF:
		p.errorf("unexpected end of expression")
		return litNode{}
	default:
		p.errorf("unexpected %q", t.text)
		return litNode{}
	}
}

func knownFunc(name string) bool {
	switch name {
	case "len", "lower", "upper", "min", "max", "abs", "count":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// AST and evaluation
// ---------------------------------------------------------------------------

type node interface {
	eval(env Env) (any, error)
}

type litNode struct{ val any }

func (n litNode) eval(Env) (any, error) { return n.val, nil }

type varNode struct{ name string }

func (n varNode) eval(env Env) (any, error) {
	if env == nil {
		return nil, nil
	}
	v, _ := env.Lookup(n.name)
	return normalize(v), nil
}

type listNode struct{ items []node }

func (n listNode) eval(env Env) (any, error) {
	out := make([]any, 0, len(n.items))
	for _, it := range n.items {
		v, err := it.eval(env)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

type notNode struct{ inner node }

func (n notNode) eval(env Env) (any, error) {
	v, err := n.inner.eval(env)
	if err != nil {
		return nil, err
	}
	return !truthy(v), nil
}

type negNode struct{ inner node }

func (n negNode) eval(env Env) (any, error) {
	v, err := n.inner.eval(env)
	if err != nil {
		return nil, err
	}
	f, ok := toNumber(v)
	if !ok {
		return nil, fmt.Errorf("cannot negate %v", v)
	}
	return -f, nil
}

type callNode struct {
	name string
	args []node
}

func (n callNode) eval(env Env) (any, error) {
	args := make([]any, 0, len(n.args))
	for _, a := range n.args {
		v, err := a.eval(env)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	switch n.name {
	case "len", "count":
		if len(args) != 1 {
			return nil, fmt.Errorf("%s takes one argument", n.name)
		}
		switch v := args[0].(type) {
		case []any:
			return float64(len(v)), nil
		case string:
			return float64(len(v)), nil
		case nil:
			return float64(0), nil
		}
		return nil, fmt.Errorf("%s: unsupported argument %v", n.name, args[0])
	case "lower", "upper":
		if len(args) != 1 {
			return nil, fmt.Errorf("%s takes one argument", n.name)
		}
		s := toString(args[0])
		if n.name == "lower" {
			return strings.ToLower(s), nil
		}
		return strings.ToUpper(s), nil
	case "abs":
		if len(args) != 1 {
			return nil, fmt.Errorf("abs takes one argument")
		}
		f, ok := toNumber(args[0])
		if !ok {
			return nil, fmt.Errorf("abs: not a number: %v", args[0])
		}
		return math.Abs(f), nil
	case "min", "max":
		if len(args) == 0 {
			return nil, fmt.Errorf("%s needs at least one argument", n.name)
		}
		best, ok := toNumber(args[0])
		if !ok {
			return nil, fmt.Errorf("%s: not a number: %v", n.name, args[0])
		}
		for _, a := range args[1:] {
			f, ok := toNumber(a)
			if !ok {
				return nil, fmt.Errorf("%s: not a number: %v", n.name, a)
			}
			if n.name == "min" && f < best || n.name == "max" && f > best {
				best = f
			}
		}
		return best, nil
	}
	return nil, fmt.Errorf("unknown function %q", n.name)
}

type binNode struct {
	op          string
	left, right node
}

func (n binNode) eval(env Env) (any, error) {
	// Short-circuit logic first, so an undefined right-hand side never errors when the
	// left already decided the outcome.
	switch n.op {
	case "&&":
		l, err := n.left.eval(env)
		if err != nil {
			return nil, err
		}
		if !truthy(l) {
			return false, nil
		}
		r, err := n.right.eval(env)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	case "||":
		l, err := n.left.eval(env)
		if err != nil {
			return nil, err
		}
		if truthy(l) {
			return true, nil
		}
		r, err := n.right.eval(env)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	}

	l, err := n.left.eval(env)
	if err != nil {
		return nil, err
	}
	r, err := n.right.eval(env)
	if err != nil {
		return nil, err
	}

	switch n.op {
	case "==":
		return equal(l, r), nil
	case "!=":
		return !equal(l, r), nil
	case "<", "<=", ">", ">=":
		c, ok := compare(l, r)
		if !ok {
			// Incomparable operands (nil, mixed types) are simply "not less than" — a
			// missing fact must not satisfy a threshold.
			return false, nil
		}
		switch n.op {
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		default:
			return c >= 0, nil
		}
	case "+", "-", "*", "/", "%":
		lf, lok := toNumber(l)
		rf, rok := toNumber(r)
		if n.op == "+" && (!lok || !rok) {
			return toString(l) + toString(r), nil
		}
		if !lok || !rok {
			return nil, fmt.Errorf("arithmetic on non-numbers: %v %s %v", l, n.op, r)
		}
		switch n.op {
		case "+":
			return lf + rf, nil
		case "-":
			return lf - rf, nil
		case "*":
			return lf * rf, nil
		case "/":
			if rf == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			return lf / rf, nil
		default:
			if rf == 0 {
				return nil, fmt.Errorf("modulo by zero")
			}
			return math.Mod(lf, rf), nil
		}
	case "has":
		// list has value
		list, _ := l.([]any)
		for _, item := range list {
			if equal(item, r) {
				return true, nil
			}
		}
		return false, nil
	case "in":
		// value in list
		list, _ := r.([]any)
		for _, item := range list {
			if equal(l, item) {
				return true, nil
			}
		}
		return false, nil
	case "contains":
		return strings.Contains(strings.ToLower(toString(l)), strings.ToLower(toString(r))), nil
	case "startswith":
		return strings.HasPrefix(toString(l), toString(r)), nil
	case "endswith":
		return strings.HasSuffix(toString(l), toString(r)), nil
	case "matches":
		return matchPattern(toString(l), toString(r))
	case "any", "all":
		// list any pattern / list all pattern — every element must be a string.
		list, ok := l.([]any)
		if !ok {
			if l == nil {
				return false, nil
			}
			list = []any{l}
		}
		if len(list) == 0 {
			return false, nil
		}
		for _, item := range list {
			m, err := matchPattern(toString(item), toString(r))
			if err != nil {
				return nil, err
			}
			if n.op == "any" && m {
				return true, nil
			}
			if n.op == "all" && !m {
				return false, nil
			}
		}
		return n.op == "all", nil
	}
	return nil, fmt.Errorf("unknown operator %q", n.op)
}

// matchPattern matches a string against a glob (`docs/**`, `*.go`, `internal/*/api`) or,
// with a `re:` prefix, a regular expression. Globs use `**` for any number of path segments.
func matchPattern(s, pattern string) (bool, error) {
	if rest, ok := strings.CutPrefix(pattern, "re:"); ok {
		re, err := regexp.Compile(rest)
		if err != nil {
			return false, fmt.Errorf("bad regular expression %q: %w", rest, err)
		}
		return re.MatchString(s), nil
	}
	return globMatch(pattern, s), nil
}

// globMatch is path.Match extended with `**`.
func globMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "**") {
		ok, err := path.Match(pattern, s)
		return err == nil && ok
	}
	// Split on the first ** and try every split point of s for the remainder.
	head, tail, _ := strings.Cut(pattern, "**")
	head = strings.TrimSuffix(head, "/")
	tail = strings.TrimPrefix(tail, "/")
	if head != "" {
		// The head must match a prefix of s at a segment boundary.
		if !globPrefix(head, s) {
			return false
		}
		s = strings.TrimPrefix(s, matchedPrefix(head, s))
		s = strings.TrimPrefix(s, "/")
	}
	if tail == "" {
		return true
	}
	segs := strings.Split(s, "/")
	for i := 0; i <= len(segs); i++ {
		rest := strings.Join(segs[i:], "/")
		if globMatch(tail, rest) {
			return true
		}
	}
	return false
}

// globPrefix reports whether the head pattern matches the leading segments of s.
func globPrefix(head, s string) bool {
	return matchedPrefix(head, s) != "" || head == ""
}

// matchedPrefix returns the segment-aligned prefix of s that head matches, or "".
func matchedPrefix(head, s string) string {
	n := strings.Count(head, "/") + 1
	segs := strings.Split(s, "/")
	if len(segs) < n {
		return ""
	}
	prefix := strings.Join(segs[:n], "/")
	if ok, err := path.Match(head, prefix); err == nil && ok {
		return prefix
	}
	return ""
}

// ---------------------------------------------------------------------------
// Value helpers
// ---------------------------------------------------------------------------

// normalize converts Go values from fact providers into the evaluator's small value set:
// nil, bool, float64, string, []any.
func normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, float64, string:
		return x
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalize(item)
		}
		return out
	case fmt.Stringer:
		return x.String()
	}
	// Typed string enums (domain.RiskLevel etc.) arrive as their underlying string.
	return fmt.Sprint(v)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	}
	return true
}

func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func equal(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if af, ok := a.(float64); ok {
		if bf, ok := toNumber(b); ok {
			return af == bf
		}
		return false
	}
	if bf, ok := b.(float64); ok {
		if af, ok := toNumber(a); ok {
			return af == bf
		}
		return false
	}
	if al, ok := a.([]any); ok {
		bl, ok := b.([]any)
		if !ok || len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !equal(al[i], bl[i]) {
				return false
			}
		}
		return true
	}
	// Strings and enums compare case-insensitively: "High" and "high" are the same risk.
	return strings.EqualFold(toString(a), toString(b))
}

func compare(a, b any) (int, bool) {
	if af, ok := a.(float64); ok {
		bf, ok := toNumber(b)
		if !ok {
			return 0, false
		}
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		}
		return 0, true
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		// Tiers and efforts have an order worth honouring: "T3" >= "T2", "high" > "low".
		if ar, br, ok := rankPair(as, bs); ok {
			return ar - br, true
		}
		return strings.Compare(as, bs), true
	}
	if a == nil || b == nil {
		return 0, false
	}
	af, aok2 := toNumber(a)
	bf, bok2 := toNumber(b)
	if aok2 && bok2 {
		return compare(af, bf)
	}
	return 0, false
}

var orderedEnums = [][]string{
	{"T0", "T1", "T2", "T3", "T4"},
	{"none", "low", "medium", "high", "xhigh", "max"},
	{"unknown", "low", "medium", "high", "critical"},
}

func rankPair(a, b string) (int, int, bool) {
	for _, order := range orderedEnums {
		ai, bi := -1, -1
		for i, v := range order {
			if strings.EqualFold(v, a) {
				ai = i
			}
			if strings.EqualFold(v, b) {
				bi = i
			}
		}
		if ai >= 0 && bi >= 0 {
			return ai, bi, true
		}
	}
	return 0, 0, false
}

func collectVars(n node, seen map[string]bool) {
	switch x := n.(type) {
	case varNode:
		seen[x.name] = true
	case binNode:
		collectVars(x.left, seen)
		collectVars(x.right, seen)
	case notNode:
		collectVars(x.inner, seen)
	case negNode:
		collectVars(x.inner, seen)
	case listNode:
		for _, it := range x.items {
			collectVars(it, seen)
		}
	case callNode:
		for _, a := range x.args {
			collectVars(a, seen)
		}
	}
}
