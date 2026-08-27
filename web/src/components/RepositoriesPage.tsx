import React, { useState, useEffect } from 'react';
import { api } from '../api';
import { Repository } from '../types';
import { GitBranch, Plus, RefreshCw, Database, Play } from 'lucide-react';

interface Props {
  onSelectRepoForDiagnosis: (repoId: string, snapId: string) => void;
}

export const RepositoriesPage: React.FC<Props> = ({ onSelectRepoForDiagnosis }) => {
  const [repos, setRepos] = useState<Repository[]>([]);
  const [loading, setLoading] = useState(false);
  const [showAddModal, setShowAddModal] = useState(false);
  const [name, setName] = useState('');
  const [gitURL, setGitURL] = useState('');
  const [defaultRef, setDefaultRef] = useState('main');
  const [error, setError] = useState<string | null>(null);
  const [indexingRepoId, setIndexingRepoId] = useState<string | null>(null);

  useEffect(() => {
    loadRepos();
  }, []);

  const loadRepos = async () => {
    try {
      setLoading(true);
      const list = await api.listRepositories();
      setRepos(list || []);
    } catch (err: any) {
      setError(err.message || '加载仓库失败');
    } finally {
      setLoading(false);
    }
  };

  const handleCreateRepo = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      await api.createRepository({ name, git_url: gitURL, default_ref: defaultRef });
      setShowAddModal(false);
      setName('');
      setGitURL('');
      loadRepos();
    } catch (err: any) {
      setError(err.message || '添加仓库失败');
    }
  };

  const handleTriggerIndex = async (repoId: string, ref: string) => {
    setIndexingRepoId(repoId);
    setError(null);
    try {
      await api.triggerIndex(repoId, ref);
      // Refresh list after triggering
      await loadRepos();
    } catch (err: any) {
      setError(err.message || '启动仓库索引失败');
    } finally {
      setIndexingRepoId(null);
    }
  };

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.5rem' }}>
        <div>
          <h1 style={{ fontSize: '1.75rem', fontWeight: 700, color: 'var(--text-bright)' }}>仓库</h1>
          <p style={{ color: 'var(--text-muted)', fontSize: '0.9rem' }}>
            管理 Git 仓库及其物化快照。
          </p>
        </div>
        <button className="btn btn-primary" onClick={() => setShowAddModal(true)}>
          <Plus size={16} /> 添加仓库
        </button>
      </div>

      {error && (
        <div style={{ padding: '0.75rem', background: 'rgba(248,81,73,0.15)', border: '1px solid rgba(248,81,73,0.3)', borderRadius: 6, color: 'var(--accent-danger)', marginBottom: '1rem', fontSize: '0.85rem' }}>
          {error}
        </div>
      )}

      {loading ? (
        <div style={{ textAlign: 'center', padding: '3rem', color: 'var(--text-muted)' }}>
          <RefreshCw size={24} className="spin" />
        </div>
      ) : repos.length === 0 ? (
        <div className="card" style={{ textAlign: 'center', padding: '3rem 1rem' }}>
          <Database size={36} color="var(--text-muted)" style={{ margin: '0 auto 1rem' }} />
          <h3 style={{ fontSize: '1.1rem', color: 'var(--text-bright)', marginBottom: '0.5rem' }}>暂无仓库</h3>
          <p style={{ color: 'var(--text-muted)', fontSize: '0.875rem', marginBottom: '1.5rem' }}>
            添加 Git 仓库地址，或在设置中点击 <strong>试用 Demo</strong> 浏览示例代码。
          </p>
          <button className="btn btn-primary" onClick={() => setShowAddModal(true)}>
            <Plus size={16} /> 添加第一个仓库
          </button>
        </div>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
          {repos.map((r) => (
            <div key={r.id} className="card">
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
                <div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.25rem' }}>
                    <h3 style={{ fontSize: '1.15rem', fontWeight: 600, color: 'var(--text-bright)' }}>{r.name}</h3>
                    <span className="badge badge-info">{r.status}</span>
                  </div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: '1rem', color: 'var(--text-muted)', fontSize: '0.825rem' }}>
                    <span style={{ display: 'flex', alignItems: 'center', gap: '0.25rem' }}>
                      <GitBranch size={14} /> {r.default_ref}
                    </span>
                    <span>{r.git_url}</span>
                  </div>
                  {r.snapshots && r.snapshots.length > 0 && (
                    <div style={{ marginTop: '0.5rem', color: 'var(--text-muted)', fontSize: '0.75rem' }}>
                      最新快照：<code>{r.snapshots[0].id}</code> · {r.snapshots[0].status}
                    </div>
                  )}
                </div>

                <div style={{ display: 'flex', gap: '0.5rem' }}>
                  <button
                    className="btn"
                    onClick={() => handleTriggerIndex(r.id, r.default_ref)}
                    disabled={indexingRepoId === r.id}
                  >
                    {indexingRepoId === r.id ? <RefreshCw size={14} className="spin" /> : <RefreshCw size={14} />}
                    物化并索引
                  </button>
                  <button
                    className="btn btn-primary"
                    onClick={() => onSelectRepoForDiagnosis(r.id, r.snapshots?.find((snapshot) => snapshot.status === 'READY')?.id || '')}
                    disabled={!r.snapshots?.some((snapshot) => snapshot.status === 'READY')}
                  >
                    <Play size={14} /> 新建诊断
                  </button>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* Add Repository Modal */}
      {showAddModal && (
        <div style={{
          position: 'fixed', top: 0, left: 0, right: 0, bottom: 0,
          background: 'rgba(0,0,0,0.7)', display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 100
        }}>
          <div className="card" style={{ width: 500, maxWidth: '90%' }}>
            <h2 style={{ fontSize: '1.25rem', color: 'var(--text-bright)', marginBottom: '1rem' }}>添加 Git 仓库</h2>
            <form onSubmit={handleCreateRepo}>
              <div className="form-group">
                <label className="form-label">仓库名称</label>
                <input
                  type="text"
                  className="input-field"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="例如：auth-service"
                  required
                />
              </div>

              <div className="form-group">
                <label className="form-label">Git HTTPS 地址</label>
                <input
                  type="url"
                  className="input-field"
                  value={gitURL}
                  onChange={(e) => setGitURL(e.target.value)}
                  placeholder="https://github.com/org/repo.git"
                  required
                />
                <div className="form-hint">仅支持 HTTPS，并防护 SSRF 与私有 IP。</div>
              </div>

              <div className="form-group">
                <label className="form-label">默认分支 / Ref</label>
                <input
                  type="text"
                  className="input-field"
                  value={defaultRef}
                  onChange={(e) => setDefaultRef(e.target.value)}
                  placeholder="main"
                  required
                />
              </div>

              <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '0.5rem', marginTop: '1.5rem' }}>
                <button type="button" className="btn" onClick={() => setShowAddModal(false)}>
                  取消
                </button>
                <button type="submit" className="btn btn-primary">
                  创建仓库
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  );
};
