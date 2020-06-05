package pointer

import (
	"time"
)

func Int32(v int32) *int32 {
	return &v
}

func String(v string) *string {
	return &v
}

func Duration(v time.Duration) *time.Duration {
	return &v
}
