package mysql

import (
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
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
		&codeintelmodel.CodeIndexBuild{},
		&codeintelmodel.RetrievalBuild{},
		&codeintelmodel.CodeFile{},
		&codeintelmodel.Symbol{},
		&codeintelmodel.SymbolRelation{},
		&repoindex.RepositoryIndex{},
		&diagnosis.DiagnosisRun{},
		&diagnosis.DiagnosisAttempt{},
		&evidence.Report{},
		&evidence.Citation{},
		&trace.AgentStep{},
		&jobs.AnalysisJob{},
	)
}
