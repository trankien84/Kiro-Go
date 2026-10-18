package proxy

import (
	"errors"
	"fmt"
)

const completionGuardDefaultMaxBufferedBytes = 16 * 1024 * 1024

var errCompletionGuardBufferLimit = errors.New(
	"completion guard response exceeded the configured buffer limit",
)

type completionGuardRetainedBudget struct {
	limit    int
	retained int
	err      error
}

func newCompletionGuardRetainedBudget(limit int) *completionGuardRetainedBudget {
	return &completionGuardRetainedBudget{limit: limit}
}

func (b *completionGuardRetainedBudget) reserve(size int) bool {
	if b == nil || size <= 0 {
		return true
	}
	if b.err != nil {
		return false
	}
	if b.limit > 0 &&
		(b.retained >= b.limit || size > b.limit-b.retained) {
		b.err = fmt.Errorf(
			"%w: retained=%d requested=%d limit=%d bytes",
			errCompletionGuardBufferLimit,
			b.retained,
			size,
			b.limit,
		)
		return false
	}
	b.retained += size
	return true
}

func (b *completionGuardRetainedBudget) release(size int) {
	if b == nil || size <= 0 {
		return
	}
	b.retained -= size
	if b.retained < 0 {
		b.retained = 0
	}
}

func (b *completionGuardRetainedBudget) Err() error {
	if b == nil {
		return nil
	}
	return b.err
}

type completionGuardRetainedBudgetProvider interface {
	completionGuardBudget() *completionGuardRetainedBudget
}
