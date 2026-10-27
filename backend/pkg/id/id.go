package id

import (
	"crypto/rand"
	"fmt"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
)

// New returns a fresh monotonic ULID as a string.
func New() string {
	entropy := ulid.Monotonic(rand.Reader, 0)
	id, _ := ulid.New(ulid.Timestamp(time.Now()), entropy)
	return id.String()
}

// NewWorkerID returns host:pid, identifying this process.
func NewWorkerID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}
