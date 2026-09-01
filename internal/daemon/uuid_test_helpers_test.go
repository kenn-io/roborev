package daemon

import (
	"crypto/sha256"
	"uuid"
)

func testUUID(label string) uuid.UUID {
	sum := sha256.Sum256([]byte(label))
	var id uuid.UUID
	copy(id[:], sum[:len(id)])
	return id
}

func testUUIDPtr(label string) *uuid.UUID {
	return new(testUUID(label))
}
