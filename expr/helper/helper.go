package helper

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-graphite/carbonapi/expr/interfaces"
	"github.com/go-graphite/carbonapi/expr/types"
	"github.com/go-graphite/carbonapi/pkg/parser"
)

var evaluator interfaces.Evaluator

// Backref is a pre-compiled expression for backref
var Backref = regexp.MustCompile(`\\(\d+)`)

// ErrUnknownFunction is an error message about unknown function
type ErrUnknownFunction string

func (e ErrUnknownFunction) Error() string {
	return fmt.Sprintf("unknown function in evalExpr: %q", string(e))
}

// SetEvaluator sets evaluator for all helper functions
func SetEvaluator(e interfaces.Evaluator) {
	evaluator = e
}

// GetSeriesArg returns argument from series.
func GetSeriesArg(arg parser.Expr, from, until int64, values map[parser.MetricRequest][]*types.MetricData) ([]*types.MetricData, error) {
	if !arg.IsName() && !arg.IsFunc() {
		return nil, parser.ErrMissingTimeseries
	}

	a, err := evaluator.Eval(arg, from, until, values)
	if err != nil {
		return nil, err
	}

	return a, nil
}

// RemoveEmptySeriesFromName removes empty series from list of names.
func RemoveEmptySeriesFromName(args []*types.MetricData) string {
	var argNames []string
	for _, arg := range args {
		argNames = append(argNames, arg.Name)
	}

	return strings.Join(argNames, ",")
}

// GetSeriesArgs returns arguments of series
func GetSeriesArgs(e []parser.Expr, from, until int64, values map[parser.MetricRequest][]*types.MetricData) ([]*types.MetricData, error) {
	var args []*types.MetricData

	for _, arg := range e {
		a, err := GetSeriesArg(arg, from, until, values)
		if err != nil && err != parser.ErrSeriesDoesNotExist {
			return nil, err
		}
		args = append(args, a...)
	}

	if len(args) == 0 {
		return nil, parser.ErrSeriesDoesNotExist
	}

	return args, nil
}

// GetSeriesArgsAndRemoveNonExisting will fetch all required arguments, but will also filter out non existing Series
// This is needed to be graphite-web compatible in cases when you pass non-existing Series to, for example, sumSeries
func GetSeriesArgsAndRemoveNonExisting(e parser.Expr, from, until int64, values map[parser.MetricRequest][]*types.MetricData) ([]*types.MetricData, error) {
	args, err := GetSeriesArgs(e.Args(), from, until, values)
	if err != nil {
		return nil, err
	}

	// We need to rewrite name if there are some missing metrics
	if len(args) < len(e.Args()) {
		e.SetRawArgs(RemoveEmptySeriesFromName(args))
	}

	return args, nil
}

type seriesFunc func(*types.MetricData, *types.MetricData) *types.MetricData

// ForEachSeriesDo do action for each serie in list.
func ForEachSeriesDo(e parser.Expr, from, until int64, values map[parser.MetricRequest][]*types.MetricData, function seriesFunc) ([]*types.MetricData, error) {
	arg, err := GetSeriesArg(e.Args()[0], from, until, values)
	if err != nil {
		return nil, parser.ErrMissingTimeseries
	}
	var results []*types.MetricData

	for _, a := range arg {
		r := *a
		r.Name = fmt.Sprintf("%s(%s)", e.Target(), a.Name)
		r.Values = make([]float64, len(a.Values))
		results = append(results, function(a, &r))
	}
	return results, nil
}

// AlignSeries aligns different series together. By default it only prepends and appends NaNs in case of different length, but if ExtrapolatePoints is enabled, it can extrapolate
func AlignSeries(args []*types.MetricData) []*types.MetricData {
	minStart := args[0].StartTime
	maxStop := args[0].StopTime
	maxVals := 0
	minStepTime := args[0].StepTime
	for j := 0; j < 2; j++ {
		if ExtrapolatePoints {
			for _, arg := range args {
				if arg.StepTime < minStepTime {
					minStepTime = arg.StepTime
				}

				if arg.StepTime > minStepTime {
					valsCnt := int(math.Ceil(float64(arg.StopTime-arg.StartTime) / float64(minStepTime)))
					newVals := make([]float64, valsCnt)
					ts := arg.StartTime
					nextTs := arg.StartTime + arg.StepTime
					i := 0
					j := 0
					pointsPerInterval := float64(ts-nextTs) / float64(minStepTime)
					v := arg.Values[0]
					dv := (arg.Values[0] - arg.Values[1]) / pointsPerInterval
					for ts < arg.StopTime {
						newVals[i] = v
						v += dv
						if ts > nextTs {
							j++
							nextTs += arg.StepTime
							v = arg.Values[j]
							dv = (arg.Values[j-1] - v) / pointsPerInterval
						}
						ts += minStepTime
						i++
					}
					arg.Values = newVals
					arg.StepTime = minStepTime
				}
			}
		}

		for _, arg := range args {
			if len(arg.Values) > maxVals {
				maxVals = len(arg.Values)
			}
			if arg.StartTime < minStart {
				minStart = arg.StartTime
			}
			if minStart < arg.StartTime {
				valCnt := (arg.StartTime - minStart) / arg.StepTime
				newVals := make([]float64, valCnt)
				newVals = append(newVals, arg.Values...)
				arg.Values = newVals
				arg.StartTime = minStart
			}

			if arg.StopTime > maxStop {
				maxStop = arg.StopTime
			}
			if maxStop > arg.StopTime {
				valCnt := (maxStop - arg.StopTime) / arg.StepTime
				newVals := make([]float64, valCnt)
				arg.Values = append(arg.Values, newVals...)
				arg.StopTime = maxStop
			}
		}
	}
	return args
}

// AggregateFunc type that defined aggregate function
type AggregateFunc func([]float64) float64

// AggregateSeries aggregates series
func AggregateSeries(e parser.Expr, args []*types.MetricData, function AggregateFunc) ([]*types.MetricData, error) {
	args = AlignSeries(args)
	length := len(args[0].Values)
	r := *args[0]
	r.Name = fmt.Sprintf("%s(%s)", e.Target(), e.RawArgs())
	r.Values = make([]float64, length)

	for i := range args[0].Values {
		var values []float64
		for _, arg := range args {
			values = append(values, arg.Values[i])
		}

		r.Values[i] = math.NaN()
		if len(values) > 0 {
			r.Values[i] = function(values)
		}
	}

	return []*types.MetricData{&r}, nil
}

// ExtractMetric extracts metric out of function list
func ExtractMetric(s string) string {

	// search for a metric name in 's'
	// metric name is defined to be a Series of name characters terminated by a ',' or ')'
	// work sample: bla(bla{bl,a}b[la,b]la) => bla{bl,a}b[la

	var (
		start, braces, i, w int
		r                   rune
	)

FOR:
	for braces, i, w = 0, 0, 0; i < len(s); i += w {

		w = 1
		if parser.IsNameChar(s[i]) {
			continue
		}

		switch s[i] {
		// If metric name have tags, we want to skip them
		case ';':
			break FOR
		case '{':
			braces++
		case '}':
			if braces == 0 {
				break FOR
			}
			braces--
		case ',':
			if braces == 0 {
				break FOR
			}
		case ')':
			break FOR
		default:
			r, w = utf8.DecodeRuneInString(s[i:])
			if unicode.In(r, parser.RangeTables...) {
				continue
			}
			start = i + 1
		}

	}

	return s[start:i]
}

// Contains check if slice 'a' contains value 'i'
func Contains(a []int, i int) bool {
	for _, aa := range a {
		if aa == i {
			return true
		}
	}
	return false
}
