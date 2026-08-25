package elasticsearch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"repolens/internal/indexing"
	"repolens/internal/platform/elasticsearch"
)

func TestClient_BulkIndexChunks_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "_bulk") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"took": 15,
				"errors": false,
				"items": [
					{
						"index": {
							"_index": "test_chunks",
							"_id": "chunk-001",
							"status": 201,
							"result": "created"
						}
					},
					{
						"index": {
							"_index": "test_chunks",
							"_id": "chunk-002",
							"status": 201,
							"result": "created"
						}
					}
				]
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := elasticsearch.NewClient(server.URL, "test_chunks")
	chunks := []indexing.CodeChunk{
		{ID: "chunk-001", SnapshotID: "snap-1", Path: "main.go", Content: "package main"},
		{ID: "chunk-002", SnapshotID: "snap-1", Path: "util.go", Content: "package util"},
	}

	err := client.BulkIndexChunks(context.Background(), chunks, nil)
	if err != nil {
		t.Fatalf("expected nil error on bulk success, got: %v", err)
	}
}

func TestClient_BulkIndexChunks_PartialFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "_bulk") {
			// Elastic returns HTTP 200 OK even when some items have errors
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"took": 25,
				"errors": true,
				"items": [
					{
						"index": {
							"_index": "test_chunks",
							"_id": "chunk-success",
							"status": 201,
							"result": "created"
						}
					},
					{
						"index": {
							"_index": "test_chunks",
							"_id": "chunk-failed",
							"status": 400,
							"error": {
								"type": "mapper_parsing_exception",
								"reason": "failed to parse field [embedding] of type [dense_vector]"
							}
						}
					}
				]
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := elasticsearch.NewClient(server.URL, "test_chunks")
	chunks := []indexing.CodeChunk{
		{ID: "chunk-success", SnapshotID: "snap-1", Path: "main.go", Content: "package main"},
		{ID: "chunk-failed", SnapshotID: "snap-1", Path: "bad.go", Content: "package bad"},
	}

	err := client.BulkIndexChunks(context.Background(), chunks, nil)
	if err == nil {
		t.Fatalf("expected error on bulk partial failure (errors: true), got nil")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "partial failure") {
		t.Errorf("expected error message to mention partial failure, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "chunk-failed") {
		t.Errorf("expected error message to contain failed chunk ID 'chunk-failed', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "status=400") {
		t.Errorf("expected error message to contain status 400, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "mapper_parsing_exception") {
		t.Errorf("expected error message to contain error type 'mapper_parsing_exception', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "failed to parse field") {
		t.Errorf("expected error message to contain error reason, got: %s", errMsg)
	}
}

func TestClient_BulkIndexChunks_EmptyChunks(t *testing.T) {
	client := elasticsearch.NewClient("http://localhost:9200", "test_chunks")
	err := client.BulkIndexChunks(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("expected nil error for empty chunks, got: %v", err)
	}
}
