package pointer

// String returns a reference to a string.
func String(s string) *string {
	return &s
}

// Int32 returns a reference to int32.
func Int32(n int32) *int32 {
	return &n
}
