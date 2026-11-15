package claude

import (
	"sync"
	"testing"
)

func TestReadMetadataBool(t *testing.T) {
	var empty map[string]any
	if ReadMetadataBool(nil, "flag") || ReadMetadataBool(&empty, "flag") {
		t.Fatal("nil metadata returned true")
	}
	metadata := map[string]any{"true": true, "false": false, "string": "true"}
	for key, want := range map[string]bool{"true": true, "false": false, "string": false, "missing": false} {
		if got := ReadMetadataBool(&metadata, key); got != want {
			t.Fatalf("ReadMetadataBool(%q) = %t, want %t", key, got, want)
		}
	}
}

func TestReadMetadataBoolConcurrentWriters(t *testing.T) {
	var metadata map[string]any
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 1000 {
				StoreMetadataValue(&metadata, "is_setup_token", true)
				StoreMetadataString(&metadata, "account_uuid", "synthetic-account")
				_ = ReadMetadataBool(&metadata, "is_setup_token")
			}
		}()
	}
	workers.Wait()
	if !ReadMetadataBool(&metadata, "is_setup_token") {
		t.Fatal("stored boolean was not preserved")
	}
}
