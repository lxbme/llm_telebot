package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai/jsonschema"
)

// NOTE: Built-in tools continue to use jsonschema.Definition for convenience.
// The MCPTool.Parameters() interface returns `any`, which jsonschema.Definition satisfies.

// ─── Built-in Tools ──────────────────────────────────────────────────────────
//
// Each tool implements the MCPTool interface. To add your own:
//   1. Create a struct implementing MCPTool.
//   2. Register it in RegisterBuiltinTools() below.
//
// The LLM will automatically see and use these tools when TOOLS_ENABLED=true.

// RegisterBuiltinTools registers all built-in tools with the given registry.
// Call this once during bot initialisation.
func RegisterBuiltinTools(r *ToolRegistry) {
	r.Register(&CurrentTimeTool{})
	r.Register(&RandomNumberTool{})
	r.Register(&CalculatorTool{})
}

// ─── CurrentTimeTool ─────────────────────────────────────────────────────────

// CurrentTimeTool returns the current date and time.
type CurrentTimeTool struct{}

func (t *CurrentTimeTool) Name() string { return "get_current_time" }
func (t *CurrentTimeTool) Description() string {
	return "Get the current date and time in the specified timezone. Defaults to UTC."
}
func (t *CurrentTimeTool) Parameters() any {
	return jsonschema.Definition{
		Type: jsonschema.Object,
		Properties: map[string]jsonschema.Definition{
			"timezone": {
				Type:        jsonschema.String,
				Description: "IANA timezone name, e.g. 'Asia/Shanghai', 'America/New_York'. Defaults to 'UTC'.",
			},
		},
	}
}

func (t *CurrentTimeTool) Execute(args string) (string, error) {
	var params struct {
		Timezone string `json:"timezone"`
	}
	if args != "" && args != "{}" {
		if err := ParseArgs(args, &params); err != nil {
			return "", err
		}
	}
	tz := params.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return "", fmt.Errorf("unknown timezone %q: %w", tz, err)
	}
	now := time.Now().In(loc)
	return now.Format("2006-01-02 15:04:05 (Monday) MST"), nil
}

// ─── RandomNumberTool ────────────────────────────────────────────────────────

// RandomNumberTool generates a random integer in [min, max].
type RandomNumberTool struct{}

func (t *RandomNumberTool) Name() string { return "random_number" }
func (t *RandomNumberTool) Description() string {
	return "Generate a random integer between min and max (inclusive)."
}
func (t *RandomNumberTool) Parameters() any {
	return jsonschema.Definition{
		Type: jsonschema.Object,
		Properties: map[string]jsonschema.Definition{
			"min": {
				Type:        jsonschema.Integer,
				Description: "Lower bound (inclusive). Default: 1.",
			},
			"max": {
				Type:        jsonschema.Integer,
				Description: "Upper bound (inclusive). Default: 100.",
			},
		},
	}
}

func (t *RandomNumberTool) Execute(args string) (string, error) {
	var params struct {
		Min *int `json:"min"`
		Max *int `json:"max"`
	}
	if args != "" && args != "{}" {
		if err := ParseArgs(args, &params); err != nil {
			return "", err
		}
	}
	lo, hi := 1, 100
	if params.Min != nil {
		lo = *params.Min
	}
	if params.Max != nil {
		hi = *params.Max
	}
	if lo > hi {
		lo, hi = hi, lo
	}
	n := lo + rand.Intn(hi-lo+1)
	return fmt.Sprintf("%d", n), nil
}

// ─── CalculatorTool ──────────────────────────────────────────────────────────

// CalculatorTool evaluates simple arithmetic expressions.
type CalculatorTool struct{}

func (t *CalculatorTool) Name() string { return "calculator" }
func (t *CalculatorTool) Description() string {
	return "Evaluate a simple arithmetic expression. Supports: + - * / ^ sqrt. Example: '2 + 3 * 4', 'sqrt(144)', '2^10'."
}
func (t *CalculatorTool) Parameters() any {
	return jsonschema.Definition{
		Type: jsonschema.Object,
		Properties: map[string]jsonschema.Definition{
			"expression": {
				Type:        jsonschema.String,
				Description: "The arithmetic expression to evaluate.",
			},
		},
		Required: []string{"expression"},
	}
}

func (t *CalculatorTool) Execute(args string) (string, error) {
	var params struct {
		Expression string `json:"expression"`
	}
	if err := ParseArgs(args, &params); err != nil {
		return "", err
	}
	expr := strings.TrimSpace(params.Expression)
	if expr == "" {
		return "", fmt.Errorf("empty expression")
	}
	result, err := evalExpr(expr)
	if err != nil {
		return "", err
	}
	// Format nicely: drop ".0" for integer results.
	if result == math.Trunc(result) && !math.IsInf(result, 0) {
		return fmt.Sprintf("%.0f", result), nil
	}
	return fmt.Sprintf("%g", result), nil
}

// ─── Simple expression evaluator ─────────────────────────────────────────────
// Supports: numbers, +, -, *, /, ^ (power), sqrt(), parentheses.
// Recursive descent parser — no external dependency.

type exprParser struct {
	input string
	pos   int
}

func evalExpr(s string) (float64, error) {
	p := &exprParser{input: strings.ReplaceAll(s, " ", "")}
	result := p.parseExpr()
	if p.pos < len(p.input) {
		return 0, fmt.Errorf("unexpected character at position %d: %c", p.pos, p.input[p.pos])
	}
	return result, nil
}

func (p *exprParser) parseExpr() float64 {
	result := p.parseTerm()
	for p.pos < len(p.input) {
		if p.input[p.pos] == '+' {
			p.pos++
			result += p.parseTerm()
		} else if p.input[p.pos] == '-' {
			p.pos++
			result -= p.parseTerm()
		} else {
			break
		}
	}
	return result
}

func (p *exprParser) parseTerm() float64 {
	result := p.parsePower()
	for p.pos < len(p.input) {
		if p.input[p.pos] == '*' {
			p.pos++
			result *= p.parsePower()
		} else if p.input[p.pos] == '/' {
			p.pos++
			result /= p.parsePower()
		} else {
			break
		}
	}
	return result
}

func (p *exprParser) parsePower() float64 {
	result := p.parseUnary()
	if p.pos < len(p.input) && p.input[p.pos] == '^' {
		p.pos++
		exp := p.parsePower() // right-associative
		result = math.Pow(result, exp)
	}
	return result
}

func (p *exprParser) parseUnary() float64 {
	if p.pos < len(p.input) && p.input[p.pos] == '-' {
		p.pos++
		return -p.parseAtom()
	}
	return p.parseAtom()
}

func (p *exprParser) parseAtom() float64 {
	// sqrt(...)
	if p.pos+4 <= len(p.input) && strings.ToLower(p.input[p.pos:p.pos+4]) == "sqrt" {
		p.pos += 4
		if p.pos < len(p.input) && p.input[p.pos] == '(' {
			p.pos++
			val := p.parseExpr()
			if p.pos < len(p.input) && p.input[p.pos] == ')' {
				p.pos++
			}
			return math.Sqrt(val)
		}
	}

	// parenthesised sub-expression
	if p.pos < len(p.input) && p.input[p.pos] == '(' {
		p.pos++
		val := p.parseExpr()
		if p.pos < len(p.input) && p.input[p.pos] == ')' {
			p.pos++
		}
		return val
	}

	// number
	start := p.pos
	for p.pos < len(p.input) && (p.input[p.pos] >= '0' && p.input[p.pos] <= '9' || p.input[p.pos] == '.') {
		p.pos++
	}
	if start == p.pos {
		return 0
	}
	var val float64
	fmt.Sscanf(p.input[start:p.pos], "%f", &val)
	return val
}
