package mysql

import (
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/outbox"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
	"repolens/internal/user"
)

func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&user.User{},
		&repo.Repository{},
		&snapshot.RepositorySnapshot{},
		&repoindex.RepositoryIndex{},
		&outbox.OutboxEvent{},
		&diagnosis.DiagnosisRun{},
		&diagnosis.DiagnosisAttempt{},
		&evidence.Report{},
		&evidence.Citation{},
		&trace.AgentStep{},
		&jobs.AnalysisJob{},
	)
}
