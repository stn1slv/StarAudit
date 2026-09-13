package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stn1slv/staraudit/pkg/history"
)

func TestExitCode(t *testing.T) {
	assert.Equal(t, 0, exitCode(history.VerdictPass))
	assert.Equal(t, 2, exitCode(history.VerdictReview))
	assert.Equal(t, 3, exitCode(history.VerdictSuspicious))
}
