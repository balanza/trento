package lib

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShStdin(t *testing.T) {
	// Echo stdin back so we can verify it was piped correctly.
	out, _, code, err := ShStdin(context.Background(), "", "hello via stdin", "cat")
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "hello via stdin", out)
}

func TestShStdin_EmptyStdin(t *testing.T) {
	out, _, code, err := ShStdin(context.Background(), "", "", "cat")
	assert.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "", out)
}

func TestShStdin_NonZeroExit(t *testing.T) {
	_, _, code, err := ShStdin(context.Background(), "", "ignored", "sh", "-c", "exit 7")
	assert.NoError(t, err)
	assert.Equal(t, 7, code)
}

func TestShStdin_EmptyArgv(t *testing.T) {
	_, _, _, err := ShStdin(context.Background(), "", "ignored")
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "empty argv"))
}

func TestMustShStdin(t *testing.T) {
	out, err := MustShStdin(context.Background(), "", "via must", "cat")
	assert.NoError(t, err)
	assert.Equal(t, "via must", out)
}

func TestMustShStdin_NonZeroExit(t *testing.T) {
	_, err := MustShStdin(context.Background(), "", "ignored", "sh", "-c", "echo oops 1>&2; exit 3")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exited 3")
	assert.Contains(t, err.Error(), "oops")
}
