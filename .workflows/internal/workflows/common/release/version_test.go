package release

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBumpPatch(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{"2.4.7", "2.4.8", false},
		{"0.0.0", "0.0.1", false},
		{"10.20.99", "10.20.100", false},
		{"1.2", "", true},
		{"1.2.3.4", "", true},
		{"1.2.x", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := BumpPatch(c.in)
		if c.err {
			assert.Error(t, err, "input=%q", c.in)
			continue
		}
		assert.NoError(t, err, "input=%q", c.in)
		assert.Equal(t, c.want, got, "input=%q", c.in)
	}
}

func TestSplitOwnerName(t *testing.T) {
	cases := []struct {
		in     string
		wantO  string
		wantN  string
		wantOK bool
	}{
		{"trento-project/web", "trento-project", "web", true},
		{"trento-project/wanda", "trento-project", "wanda", true},
		{"trento-project", "", "", false},
		{"/web", "", "", false},
		{"trento-project/", "", "", false},
		{"", "", "", false},
		{"a/b/c", "a", "b/c", true}, // First slash wins
	}
	for _, c := range cases {
		o, n, ok := SplitOwnerName(c.in)
		assert.Equal(t, c.wantOK, ok, "input=%q", c.in)
		assert.Equal(t, c.wantO, o, "input=%q", c.in)
		assert.Equal(t, c.wantN, n, "input=%q", c.in)
	}
}

func TestDecodeBase64Trim(t *testing.T) {
	// echo -n "2.4.7" | base64  →  Mi40Ljc=
	got, err := DecodeBase64Trim("Mi40Ljc=")
	assert.NoError(t, err)
	assert.Equal(t, "2.4.7", got)
	got, err = DecodeBase64Trim("  Mi40Ljc= \n")
	assert.NoError(t, err)
	assert.Equal(t, "2.4.7", got)
	_, err = DecodeBase64Trim("not-base64")
	assert.Error(t, err)
}
