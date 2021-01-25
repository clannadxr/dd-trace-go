// Package stackparse is an experimental replacement for pkg panicparse. The
// goal for the parser is to be:
//
// 1. Safe: No panics should be thrown.
// 2. Simple: Keep this pkg small and easy to modify.
// 3. Forgiving: Favor producing partial results over no results, even if the
// input data is different than expected.
// 4. Efficient: Try to be at least 10x faster than panicparse as long as it
// doesn't complicate things.
//
// Before implementing this pkg we experimented with existing libraries such
// panicparse, but decided to roll out our own due to complications with go
// modules, performance (stackparse is 100x faster) and complexity (stackparse
// is 20x less code).
//
// [1] https://github.com/maruel/panicparse/issues/57
package stackparse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type parserState int

const (
	stateHeader = iota
	stateStackFunc
	stateStackFile
	stateCreatedBy
	stateCreatedByFunc
	stateCreatedByFile
)

// Parse parses a goroutines stack trace dump as produced by runtime.Stack().
// The parser is forgiving and will continue parsing even when encountering
// unexpected data. When this happens it will try to discard the entire
// goroutine that encountered the problem and continue with the next one. It
// will also return an *Error including an error for every goroutine that
// couldn't be parsed. If all goroutines were parsed successfully, *Errors is
// nil.
//
// Known Issues:
// 1. The max stack depth for runtime.Stack() is 100. Goroutines with a deeper
// stack will have "...additional frames elided...\n" in their output, which is
// currently treated as a parse error which means the goroutine is not included
// in the return value.
//
func Parse(r io.Reader) ([]*Goroutine, *Errors) {
	var (
		sc      = bufio.NewScanner(r)
		state   parserState
		lineNum int
		line    []byte

		goroutines []*Goroutine
		g          *Goroutine
		f          *Frame

		errs           = &Errors{}
		abortGoroutine = func(msg string) {
			err := fmt.Errorf(
				"%s on line %d: %q",
				msg,
				lineNum,
				line,
			)
			errs.Errors = append(errs.Errors, err)
			goroutines = goroutines[0 : len(goroutines)-1]
			state = stateHeader
		}
	)

	// This for loop implements a simple line based state machine for parsing
	// the goroutines dump.
	for sc.Scan() {
		line = sc.Bytes()
		lineNum++

	statemachine:
		switch state {
		case stateHeader:
			// Ignore lines that don't look like goroutine headers. This helps with
			// leading/trailing whitespace but also in case we encountered an error
			// during the previous goroutine and need to seek to the beginning of the
			// next one.
			if !bytes.HasPrefix(line, goroutinePrefix) {
				continue
			}

			g = parseGoroutineHeader(line[len(goroutinePrefix):])
			goroutines = append(goroutines, g)
			state = stateStackFunc
			if g == nil {
				abortGoroutine("invalid goroutine header")
			}
		case stateStackFunc, stateCreatedByFunc:
			f = parseFunc(line, state)
			if f == nil {
				abortGoroutine("invalid function call")
				continue
			}
			if state == stateStackFunc {
				g.Stack = append(g.Stack, f)
				state = stateStackFile
			} else {
				g.CreatedBy = f
				state = stateCreatedByFile
			}
		case stateStackFile, stateCreatedByFile:
			if !parseFile(line, f) {
				abortGoroutine("invalid file:line ref")
				continue
			}
			state = stateCreatedBy
		// TODO(fg) rename this state? three different things might happen here
		case stateCreatedBy:
			if bytes.HasPrefix(line, createdByPrefix) {
				line = line[len(createdByPrefix):]
				state = stateCreatedByFunc
				goto statemachine
			} else if len(line) == 0 {
				state = stateHeader
			} else {
				state = stateStackFunc
				goto statemachine
			}
		}
	}

	if err := sc.Err(); err != nil {
		errs.Errors = append(errs.Errors, err)
	}
	if len(errs.Errors) > 0 {
		return goroutines, errs
	}
	return goroutines, nil
}

var (
	goroutinePrefix = []byte("goroutine ")
	createdByPrefix = []byte("created by ")
)

var goroutineHeader = regexp.MustCompile(
	`^(\d+) \[([^,]+)(?:, (\d+) minutes)?(, locked to thread)?\]:$`,
)

// parseGoroutineHeader parses a goroutine header line and returns a new
// Goroutine on success or nil on error.
//
// Example Input:
// goroutine 1 [chan receive, 6883 minutes]:
//
// Example Output:
// &Goroutine{ID: 1, State "chan receive", Waitduration: 6883*time.Minute}
func parseGoroutineHeader(line []byte) *Goroutine {
	// TODO(fg) would probably be faster if we didn't use a regexp for this, but
	// might be more hassle than its worth.
	m := goroutineHeader.FindSubmatch(line)
	if len(m) != 5 {
		return nil
	}
	var (
		id          = m[1]
		state       = m[2]
		waitminutes = m[3]
		locked      = m[4]

		g   = &Goroutine{State: string(state), LockedToThread: len(locked) > 0}
		err error
	)

	// regex currently sucks `abc minutes` into the state string if abc is
	// non-numeric, let's not consider this a valid goroutine.
	if strings.HasSuffix(g.State, " minutes") {
		return nil
	} else if g.ID, err = strconv.Atoi(string(id)); err != nil {
		// should be impossible to end up here
		return nil
	} else if len(waitminutes) == 0 {
		// do nothing, goroutine isn't waiting
	} else if min, err := strconv.Atoi(string(waitminutes)); err != nil {
		// should be impossible to end up here
		return nil
	} else {
		g.Wait = time.Duration(min) * time.Minute
	}
	return g
}

// parseFunc parse a func call with potential argument addresses and returns a
// new Frame for it on success or nil on error.
//
// Example Input:
// runtime/pprof.writeGoroutineStacks(0x2b016e0, 0xc0995cafc0, 0xc00468e150, 0x0)
//
// Example Output:
// &Frame{Func: "runtime/pprof.writeGoroutineStacks"}
func parseFunc(line []byte, state parserState) *Frame {
	if state == stateCreatedByFunc {
		return &Frame{Func: string(line)}
	}

	// A valid func call is supposed to have at least one matched pair of parens.
	// Multiple matched pairs are allowed, but nesting is not.
	var (
		openIndex  = -1
		closeIndex = -1
	)
	for i, r := range line {
		switch r {
		case '(':
			if openIndex != -1 && closeIndex == -1 {
				return nil
			}
			openIndex = i
			closeIndex = -1
		case ')':
			if openIndex == -1 || closeIndex != -1 {
				return nil
			}
			closeIndex = i
		}
	}

	if openIndex == -1 || closeIndex == -1 || openIndex == 0 {
		return nil
	}
	return &Frame{Func: string(line[0:openIndex])}
}

// parseFile parses a file line and updates f accordingly or returns false on
// error.
//
// Example Input:
// /root/go1.15.6.linux.amd64/src/net/http/server.go:2969 +0x36c
//
// Example Update:
// &Frame{File: "/root/go1.15.6.linux.amd64/src/net/http/server.go", Line: 2969}
func parseFile(line []byte, f *Frame) bool {
	if len(line) == 0 || line[0] != '\t' {
		return false
	}

	line = line[1:]
	for i, c := range line {
		if c == ':' {
			if f.File != "" {
				return false
			}
			f.File = string(line[0:i])
		} else if c == ' ' || i+1 == len(line) {
			if f.File == "" {
				return false
			}
			var end int
			if c == ' ' {
				end = i
			} else {
				end = i + 1
			}

			var err error
			f.Line, err = strconv.Atoi(string(line[len(f.File)+1 : end]))
			return err == nil
		}
	}
	return false
}

type Goroutine struct {
	ID             int
	State          string
	Wait           time.Duration
	LockedToThread bool
	Stack          []*Frame
	CreatedBy      *Frame
}

type Frame struct {
	Func string
	Line int
	File string
}

type Errors struct {
	Errors []error
}

func (e *Errors) Error() string {
	return fmt.Sprintf("stackparse: %d errors occurred", len(e.Errors))
}