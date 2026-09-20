import React, { useState, useEffect } from 'react';
import { api } from '../api';
import { AnalysisRevision, Repository } from '../types';
import { Play } from 'lucide-react';
import { getStableDiagnosisIdempotencyKey } from '../diagnosisSubmission';

interface Props {
  initialRepoId?: string;
  initialRevisionId?: string;
  onDiagnosisCreated: (diagnosisId: string) => void;
}

export const NewDiagnosisPage: React.FC<Props> = ({ initialRepoId, initialRevisionId, onDiagnosisCreated }) => {
  const [repos, setRepos] = useState<Repository[]>([]);
  const [selectedRepoId, setSelectedRepoId] = useState(initialRepoId || '');
  const [selectedRevisionId, setSelectedRevisionId] = useState(initialRevisionId || '');
  const [revisions, setRevisions] = useState<AnalysisRevision[]>([]);
  const [issueTitle, setIssueTitle] = useState('');
  const [issueDescription, setIssueDescription] = useState('');
  const [errorLog, setErrorLog] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    loadRepos();
  }, []);

  useEffect(() => {
    if (!selectedRepoId) return;
    loadRevisions(selectedRepoId);
  }, [selectedRepoId]);

  const loadRepos = async () => {
    try {
      const list = await api.listRepositories();
      setRepos(list || []);
      if (!selectedRepoId && list?.length > 0) {
        setSelectedRepoId(list[0].id);
      }
    } catch (err: any) {
      setError(err.message || '加载仓库失败');
    }
  };

  const loadRevisions = async (repoId: string) => {
    try {
      const values = await api.listAnalysisRevisions(repoId);
      setRevisions(values);
      if (!initialRevisionId && !selectedRevisionId) {
        setSelectedRevisionId(values.find((revision) => revision.status === 'READY')?.id || '');
      }
    } catch (err: any) {
      setError(err.message || '加载分析版本失败');
    }
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedRepoId) {
      setError('请选择仓库');
      return;
    }
    if (!issueTitle) {
      setError('请填写问题标题');
      return;
    }
    if (!selectedRevisionId) {
      setError('请先选择 READY 分析版本');
      return;
    }

    setLoading(true);
    setError(null);
    try {
      const payloadFingerprint = JSON.stringify({ selectedRevisionId, issueTitle, issueDescription, errorLog });
      const idempotencyKey = getStableDiagnosisIdempotencyKey(payloadFingerprint, sessionStorage, crypto.randomUUID);
      const res = await api.createDiagnosis({
        repository_id: selectedRepoId,
        analysis_revision_id: selectedRevisionId,
        issue_title: issueTitle,
        issue_description: issueDescription,
        error_log: errorLog,
        idempotency_key: idempotencyKey,
      });
      sessionStorage.removeItem('repolens-diagnosis-payload');
      sessionStorage.removeItem('repolens-diagnosis-key');
      onDiagnosisCreated(res.diagnosis_run.id);
    } catch (err: any) {
      setError(err.message || '启动诊断失败');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div style={{ maxWidth: 800, margin: '0 auto' }}>
      <div style={{ marginBottom: '1.5rem' }}>
        <h1 style={{ fontSize: '1.75rem', fontWeight: 700, color: 'var(--text-bright)' }}>新建诊断</h1>
        <p style={{ color: 'var(--text-muted)', fontSize: '0.9rem' }}>
          提交问题标题、描述以及 CI/运行时错误堆栈，执行有代码依据的诊断。
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
            <label className="form-label">目标仓库</label>
            <select
              className="input-field"
              value={selectedRepoId}
              onChange={(e) => {
                const repoId = e.target.value;
                setSelectedRepoId(repoId);
                setSelectedRevisionId('');
              }}
              required
            >
              <option value="" disabled>选择仓库</option>
              {repos.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.name} ({r.git_url})
                </option>
              ))}
            </select>
          </div>

          <div className="form-group">
            <label className="form-label">READY 分析版本</label>
            <select className="input-field" value={selectedRevisionId} onChange={(e) => setSelectedRevisionId(e.target.value)} required>
              <option value="" disabled>选择 READY 分析版本</option>
              {revisions.filter((revision) => revision.status === 'READY').map((revision) => (
                <option key={revision.id} value={revision.id}>{revision.source_ref} · {revision.commit_sha.slice(0, 12)}</option>
              ))}
            </select>
          </div>

          <div className="form-group">
            <label className="form-label">问题标题 / Bug 摘要</label>
            <input
              type="text"
              className="input-field"
              value={issueTitle}
              onChange={(e) => setIssueTitle(e.target.value)}
              placeholder="例如：Worker channel 分发时发生 Goroutine 死锁"
              required
            />
          </div>

          <div className="form-group">
            <label className="form-label">问题描述（可选）</label>
            <textarea
              className="input-field"
              rows={3}
              value={issueDescription}
              onChange={(e) => setIssueDescription(e.target.value)}
              placeholder="描述用户看到的现象、复现步骤或上下文症状…"
            />
          </div>

          <div className="form-group">
            <label className="form-label">CI 失败 / Panic 日志 / 错误堆栈</label>
            <textarea
              className="input-field"
              rows={6}
              style={{ fontFamily: 'var(--font-mono)', fontSize: '0.825rem' }}
              value={errorLog}
              onChange={(e) => setErrorLog(e.target.value)}
              placeholder="粘贴原始错误日志、panic 堆栈或 CI 输出…"
            />
            <div className="form-hint">Authorization 请求头和 API Key 等敏感信息会在写入时自动脱敏。</div>
          </div>

          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '0.75rem', marginTop: '1.5rem' }}>
            <button type="submit" className="btn btn-primary" disabled={loading || !selectedRepoId || !selectedRevisionId || !issueTitle}>
              <Play size={16} /> {loading ? '提交中…' : '运行有依据的诊断'}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};
