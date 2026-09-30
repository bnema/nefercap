// Package uierrors holds the error helper shared by the NeferGUI adapters.
package uierrors

import (
	"context"
	"errors"
)

// Drop removes from err, and recursively from errors.Join trees, every leaf
// that is exactly one of targets. Only bare leaves match: a real failure that
// merely wraps a target (fmt.Errorf("x: %w", context.Canceled)) is kept, and so
// is every sibling of a dropped leaf. It returns nil when nothing else remains.
//
// Use it to discard the context.Canceled a NeferGUI Run reports after the
// adapter itself ended it, without hiding a real error joined to it.
func Drop(err error, targets ...error) error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var keep []error
		for _, e := range j.Unwrap() {
			if e = Drop(e, targets...); e != nil {
				keep = append(keep, e)
			}
		}
		switch len(keep) {
		case 0:
			return nil
		case 1:
			return keep[0]
		}
		return errors.Join(keep...)
	}
	for _, t := range targets {
		if err == t {
			return nil
		}
	}
	return err
}

// Ended classifies how a NeferGUI Run ended. It is pure: the caller snapshots
// its state once and passes it, so a context that ends between two reads cannot
// flip the answer.
//
// explained says that this adapter or its caller ended the run on purpose (a
// decision was made, or the parent context is done). parent is the parent
// context's error at that snapshot, nil when it is still live. closed is
// returned when the run ended without an explanation.
//
//   - explained: a bare context.Canceled (and bare parent) is the expected echo
//     of our own cancellation and is dropped. Anything else, including a real
//     error joined with it or an error that merely wraps Canceled, is kept.
//   - not explained: the run ended on its own. When something real remains
//     (an initial authorization, capability or font failure, say), exactly that
//     is returned: calling it a compositor close would mislead. Only when
//     nothing real remains, including a bare Canceled, is closed returned.
func Ended(runErr error, explained bool, parent, closed error) error {
	if explained {
		return Drop(runErr, context.Canceled, parent)
	}
	if rest := Drop(runErr, context.Canceled, parent); rest != nil {
		return rest
	}
	return closed
}

// IsCancellation reports whether err is non-nil and made only of
// context.Canceled leaves, possibly wrapped with %w or joined. A failure
// joined with, or merely mentioning, a cancellation is not one. Unlike Drop,
// wrapped Canceled counts: it is how a caller's own cancellation surfaces
// through layers that add context. DeadlineExceeded is deliberately not
// included, so a bounded-operation timeout stays a real failure.
func IsCancellation(err error) bool {
	switch e := err.(type) {
	case nil:
		return false
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		for _, child := range children {
			if !IsCancellation(child) {
				return false
			}
		}
		return len(children) > 0
	case interface{ Unwrap() error }:
		return err == context.Canceled || IsCancellation(e.Unwrap())
	default:
		return err == context.Canceled
	}
}
