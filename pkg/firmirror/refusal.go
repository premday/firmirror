package firmirror

import "errors"

// ErrRefused is what the errors of a publish that refuses to go on match: asking
// again gets the same answer until what it refused changes.
var ErrRefused = errors.New("publish refused")

// ErrRingsRefused is what the refusals to drop a ring tag match, besides
// ErrRefused: they are the ones that other rollout rings can lift.
var ErrRingsRefused = errors.New("publish refused for the rollout rings")

// refusal is an error that matches ErrRefused, and its kind if it has one, and
// keeps its own message.
type refusal struct {
	error
	kind error
}

func (r refusal) Unwrap() error { return r.error }
func (r refusal) Is(target error) bool {
	return target == ErrRefused || (r.kind != nil && target == r.kind)
}

func refuse(err error) error { return refusal{error: err} }

// refuseRings is refuse for a publish that would drop a ring tag.
func refuseRings(err error) error { return refusal{error: err, kind: ErrRingsRefused} }
