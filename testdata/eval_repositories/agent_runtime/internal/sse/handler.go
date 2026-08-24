package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"repolens/internal/diagnosis"
	"repolens/internal/platform/logger"
	"repolens/internal/trace"
)

type Event struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"`
}

type Hub struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan Event]struct{}
}

func NewHub() *Hub {
	return &Hub{
		subscribers: make(map[string]map[chan Event]struct{}),
	}
}

func (h *Hub) Subscribe(runID string) chan Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan Event, 64)
	if _, ok := h.subscribers[runID]; !ok {
		h.subscribers[runID] = make(map[chan Event]struct{})
	}
	h.subscribers[runID][ch] = struct{}{}
	return ch
}

func (h *Hub) Unsubscribe(runID string, ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if subs, ok := h.subscribers[runID]; ok {
		delete(subs, ch)
		close(ch)
		if len(subs) == 0 {
			delete(h.subscribers, runID)
		}
	}
}

func (h *Hub) Publish(runID string, event Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if subs, ok := h.subscribers[runID]; ok {
		for ch := range subs {
			select {
			case ch <- event:
			default:
				// slow consumer, drop to avoid blocking
			}
		}
	}
}

type Handler struct {
	hub            *Hub
	traceStore     trace.Store
	diagnosisStore diagnosis.Store
}

func NewHandler(hub *Hub, traceStore trace.Store, diagnosisStore diagnosis.Store) *Handler {
	return &Handler{
		hub:            hub,
		traceStore:     traceStore,
		diagnosisStore: diagnosisStore,
	}
}

func (h *Handler) Stream(c *gin.Context) {
	runID := c.Param("id")
	userID := c.GetString(string(logger.UserIDKey))

	run, err := h.diagnosisStore.GetByIDAndUser(c.Request.Context(), runID, userID)
	if err != nil || run == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "diagnosis run not found"})
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	c.Writer.Flush()

	// Check Last-Event-ID
	lastEventIDStr := c.GetHeader("Last-Event-ID")
	if lastEventIDStr == "" {
		lastEventIDStr = c.Query("last_event_id")
	}
	lastSeq, _ := strconv.Atoi(lastEventIDStr)

	// Replay previous steps if any attempt exists
	if run.FinalAttemptID != "" {
		pastSteps, err := h.traceStore.ListAfterSeq(c.Request.Context(), run.FinalAttemptID, lastSeq)
		if err == nil {
			for _, st := range pastSteps {
				dataBytes, _ := json.Marshal(st)
				fmt.Fprintf(c.Writer, "id: %d\nevent: agent.step\ndata: %s\n\n", st.Seq, string(dataBytes))
				c.Writer.Flush()
			}
		}
	}

	// If already in terminal state, send terminal event and return
	if run.Status == diagnosis.StatusSucceeded || run.Status == diagnosis.StatusFailed || run.Status == diagnosis.StatusCancelled {
		dataBytes, _ := json.Marshal(map[string]interface{}{
			"run_id": run.ID,
			"status": run.Status,
		})
		fmt.Fprintf(c.Writer, "event: diagnosis.completed\ndata: %s\n\n", string(dataBytes))
		c.Writer.Flush()
		return
	}

	eventCh := h.hub.Subscribe(runID)
	defer h.hub.Unsubscribe(runID, eventCh)

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			// Client disconnected, do not cancel background diagnosis job!
			return
		case <-ticker.C:
			// Send heartbeat comment
			fmt.Fprintf(c.Writer, ": heartbeat\n\n")
			c.Writer.Flush()
		case evt, ok := <-eventCh:
			if !ok {
				return
			}
			dataBytes, _ := json.Marshal(evt.Data)
			if evt.ID != "" {
				fmt.Fprintf(c.Writer, "id: %s\n", evt.ID)
			}
			fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", evt.Type, string(dataBytes))
			c.Writer.Flush()

			if evt.Type == "diagnosis.completed" || evt.Type == "diagnosis.failed" || evt.Type == "diagnosis.cancelled" {
				return
			}
		}
	}
}
