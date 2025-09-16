package hostnames

import (
    "testing"
    "github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
    require.Equal(t, "example.com", Normalize("Example.COM."))
}

func TestWildcardValidationAndSuffix(t *testing.T) {
    require.True(t, IsWildcard("*.example.com"))
    require.True(t, IsValidWildcard("*.example.com"))
    require.False(t, IsValidWildcard("*.com"))
    require.False(t, IsValidWildcard("example.com"))
    sfx, ok := WildcardSuffix("*.Example.COM")
    require.True(t, ok)
    require.Equal(t, ".example.com", sfx)
}

func TestFirstDotSuffix(t *testing.T) {
    sfx, ok := FirstDotSuffix("a.example.com")
    require.True(t, ok)
    require.Equal(t, ".example.com", sfx)

    _, ok = FirstDotSuffix("localhost")
    require.False(t, ok)
}

