package mysql

import (
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
)

func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&repo.Repository{},
		&snapshot.RepositorySnapshot{},
		&repoindex.RepositoryIndex{},
		&diagnosis.DiagnosisRun{},
		&diagnosis.DiagnosisAttempt{},
		&evidence.Report{},
		&evidence.Citation{},
		&trace.AgentStep{},
		&jobs.AnalysisJob{},
	)
}
