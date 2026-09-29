package trace

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAgentStepsAreAttemptScopedOrderedAndAppendOnly(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:trace-step-contract?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&AgentStep{}); err != nil {
		t.Fatal(err)
	}
	// Match the existing MySQL migration's UNIQUE(attempt_id, seq) constraint.
	if err := db.Exec("CREATE UNIQUE INDEX uq_step_attempt_seq ON agent_steps (attempt_id, seq)").Error; err != nil {
		t.Fatal(err)
	}
	store := NewStore(db)
	ctx := context.Background()
	for _, step := range []AgentStep{
		{AttemptID: "attempt-a", Seq: 3, StepType: StepTypeFinalOutput},
		{AttemptID: "attempt-b", Seq: 1, StepType: StepTypeError},
		{AttemptID: "attempt-a", Seq: 1, StepType: StepTypeThinking},
		{AttemptID: "attempt-a", Seq: 2, StepType: StepTypeToolCall},
	} {
		if err := store.Create(ctx, &step); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Create(ctx, &AgentStep{AttemptID: "attempt-a", Seq: 2, StepType: StepTypeError}); err == nil {
		t.Fatal("duplicate (attempt_id, seq) was accepted")
	}
	all, err := store.ListByAttempt(ctx, "attempt-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Seq != 1 || all[1].Seq != 2 || all[2].Seq != 3 {
		t.Fatalf("ListByAttempt order or isolation: %+v", all)
	}
	after, err := store.ListAfterSeq(ctx, "attempt-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Seq != 3 || after[0].AttemptID != "attempt-a" {
		t.Fatalf("ListAfterSeq boundary or isolation: %+v", after)
	}
}
