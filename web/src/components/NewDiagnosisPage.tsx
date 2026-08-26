import React, { useState, useEffect } from 'react';
import { api } from '../api';
import { Repository } from '../types';
import { Play } from 'lucide-react';

interface Props {
  initialRepoId?: string;
  initialSnapshotId?: string;
  onDiagnosisCreated: (diagnosisId: string) => void;
}

export const NewDiagnosisPage: React.FC<Props> = ({ initialRepoId, initialSnapshotId, onDiagnosisCreated }) => {
  const [repos, setRepos] = useState<Repository[]>([]);
  const [selectedRepoId, setSelectedRepoId] = useState(initialRepoId || '');
  const selectedSnapshotId = initialSnapshotId || '';
  const [issueTitle, setIssueTitle] = useState('');
  const [issueDescription, setIssueDescription] = useState('');
  const [errorLog, setErrorLog] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    loadRepos();
  }, []);

  const loadRepos = async () => {
    try {
      const list = await api.listRepositories();
      setRepos(list || []);
      if (!selectedRepoId && list?.length > 0) {
        setSelectedRepoId(list[0].id);
      }
    } catch {
      // ignore
    }
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedRepoId) {
      setError('Please select a repository');
      return;
    }
    if (!issueTitle) {
      setError('Please provide an issue title');
      return;
    }

    setLoading(true);
    setError(null);
    try {
      const idempotencyKey = 'idemp-ui-' + Date.now() + '-' + Math.random().toString(36).substring(2, 7);
      const res = await api.createDiagnosis({
        repository_id: selectedRepoId,
        snapshot_id: selectedSnapshotId || 'latest',
        issue_title: issueTitle,
        issue_description: issueDescription,
        error_log: errorLog,
        idempotency_key: idempotencyKey,
      });
      onDiagnosisCreated(res.diagnosis_run.id);
    } catch (err: any) {
      setError(err.message || 'Failed initiating diagnosis');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div style={{ maxWidth: 800, margin: '0 auto' }}>
      <div style={{ marginBottom: '1.5rem' }}>
        <h1 style={{ fontSize: '1.75rem', fontWeight: 700, color: 'var(--text-bright)' }}>Start New Diagnosis</h1>
        <p style={{ color: 'var(--text-muted)', fontSize: '0.9rem' }}>
          Submit an issue title, description, and CI/runtime error stack trace to execute grounded code intelligence diagnosis.
        </p>
      </div>

      {error && (
        <div style={{ padding: '0.75rem', background: 'rgba(248,81,73,0.15)', border: '1px solid rgba(248,81,73,0.3)', borderRadius: 6, color: 'var(--accent-danger)', marginBottom: '1rem', fontSize: '0.85rem' }}>
          {error}
        </div>
      )}

      <div className="card">
        <form onSubmit={handleSubmit}>
          <div className="form-group">
            <label className="form-label">Target Repository</label>
            <select
              className="input-field"
              value={selectedRepoId}
              onChange={(e) => setSelectedRepoId(e.target.value)}
              required
            >
              <option value="" disabled>Select a repository</option>
              {repos.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.name} ({r.git_url})
                </option>
              ))}
            </select>
          </div>

          <div className="form-group">
            <label className="form-label">Issue Title / Bug Summary</label>
            <input
              type="text"
              className="input-field"
              value={issueTitle}
              onChange={(e) => setIssueTitle(e.target.value)}
              placeholder="e.g. Goroutine deadlock in worker channel dispatch"
              required
            />
          </div>

          <div className="form-group">
            <label className="form-label">Issue Description (Optional)</label>
            <textarea
              className="input-field"
              rows={3}
              value={issueDescription}
              onChange={(e) => setIssueDescription(e.target.value)}
              placeholder="Describe user-reported behavior, steps to reproduce, or contextual symptoms..."
            />
          </div>

          <div className="form-group">
            <label className="form-label">CI Failure / Panic Log / Error Stacktrace</label>
            <textarea
              className="input-field"
              rows={6}
              style={{ fontFamily: 'var(--font-mono)', fontSize: '0.825rem' }}
              value={errorLog}
              onChange={(e) => setErrorLog(e.target.value)}
              placeholder="Paste raw error log, panic trace, or CI output..."
            />
            <div className="form-hint">Secrets like Authorization headers and API keys are automatically redacted on ingestion.</div>
          </div>

          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '0.75rem', marginTop: '1.5rem' }}>
            <button type="submit" className="btn btn-primary" disabled={loading || !selectedRepoId || !issueTitle}>
              <Play size={16} /> {loading ? 'Submitting...' : 'Run Grounded Diagnosis'}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};
