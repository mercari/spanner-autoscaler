package pointer

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestString(t *testing.T) {
	tests := []struct {
		title string
		str   string
	}{
		{
			title: "empty string",
			str:   "",
		},
		{
			title: "non empty string",
			str:   "s",
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.title, func(t *testing.T) {
			got := String(tc.str)
			require.Equal(t, tc.str, *got)
		})
	}
}

func TestInt32(t *testing.T) {
	num := int32(1)

	got := Int32(num)
	require.Equal(t, num, *got)
}
