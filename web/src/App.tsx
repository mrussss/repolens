import React, { useState, useEffect } from 'react';
import { SetupPage } from './components/SetupPage';
import { RepositoriesPage } from './components/RepositoriesPage';
import { NewDiagnosisPage } from './components/NewDiagnosisPage';
import { DiagnosisView } from './components/DiagnosisView';
import { CodeIntelPage } from './components/CodeIntelPage';
import { api } from './api';
import { DiagnosisRun } from './types';
import { FolderGit2, PlusCircle, Settings, Sparkles, Activity, Clock, Code2, Sun, Moon } from 'lucide-react';

type ViewMode = 'setup' | 'repos' | 'new-diag' | 'diag-view' | 'history' | 'codeintel';

export const App: React.FC = () => {
  const [currentView, setCurrentView] = useState<ViewMode>('setup');
  const [activeDiagnosisId, setActiveDiagnosisId] = useState<string | null>(null);
  const [preselectedRepoId, setPreselectedRepoId] = useState<string>('');
  const [preselectedSnapId, setPreselectedSnapId] = useState<string>('');
  const [recentDiagnoses, setRecentDiagnoses] = useState<DiagnosisRun[]>([]);
  const [theme, setTheme] = useState<'dark' | 'light'>(() => (localStorage.getItem('repolens-theme') as 'dark' | 'light') || 'dark');

  useEffect(() => { document.documentElement.dataset.theme = theme; localStorage.setItem('repolens-theme', theme); }, [theme]);

  useEffect(() => {
    loadRecentDiagnoses();
  }, [currentView]);

  const loadRecentDiagnoses = async () => {
    try {
      const list = await api.listDiagnoses();
      setRecentDiagnoses(list || []);
    } catch {
      // ignore
    }
  };

  const handleDemoStarted = (diagId: string) => {
    setActiveDiagnosisId(diagId);
    setCurrentView('diag-view');
  };

  const handleDiagnosisCreated = (diagId: string) => {
    setActiveDiagnosisId(diagId);
    setCurrentView('diag-view');
  };

  const handleSelectRepoForDiagnosis = (repoId: string, snapId: string) => {
    setPreselectedRepoId(repoId);
    setPreselectedSnapId(snapId);
    setCurrentView('new-diag');
  };

  return (
    <div style={{ minHeight: '100vh', display: 'flex', flexDirection: 'column' }}>
      {/* Top Header Navbar */}
      <header style={{ borderBottom: '1px solid var(--border-color)', background: 'var(--bg-card)', padding: '0.75rem 0' }}>
        <div className="container" style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '2rem' }}>
            <div
              style={{ display: 'flex', alignItems: 'center', gap: '0.65rem', cursor: 'pointer' }}
              onClick={() => setCurrentView('setup')}
            >
              <div style={{ background: 'var(--accent-primary)', width: 28, height: 28, borderRadius: 6, display: 'flex', alignItems: 'center', justifyContent: 'center', color: '#000', fontWeight: 800 }}>
                RL
              </div>
              <span style={{ fontSize: '1.2rem', fontWeight: 700, color: 'var(--text-bright)', letterSpacing: '-0.02em' }}>
                RepoLens
              </span>
              <span className="badge badge-info" style={{ fontSize: '0.65rem' }}>v2.1</span>
            </div>

            <nav style={{ display: 'flex', gap: '0.5rem' }}>
              <button
                className="btn"
                style={{ background: currentView === 'repos' ? 'var(--bg-subtle)' : 'transparent', border: 'none' }}
                onClick={() => setCurrentView('repos')}
              >
                <FolderGit2 size={16} /> 仓库
              </button>
              <button
                className="btn"
                style={{ background: currentView === 'codeintel' ? 'var(--bg-subtle)' : 'transparent', border: 'none' }}
                onClick={() => setCurrentView('codeintel')}
              >
                <Code2 size={16} /> 代码智能
              </button>
              <button
                className="btn"
                style={{ background: currentView === 'new-diag' ? 'var(--bg-subtle)' : 'transparent', border: 'none' }}
                onClick={() => {
                  setPreselectedRepoId('');
                  setPreselectedSnapId('');
                  setCurrentView('new-diag');
                }}
              >
                <PlusCircle size={16} /> 新建诊断
              </button>
              <button
                className="btn"
                style={{ background: currentView === 'history' ? 'var(--bg-subtle)' : 'transparent', border: 'none' }}
                onClick={() => setCurrentView('history')}
              >
                <Clock size={16} /> 历史 ({recentDiagnoses.length})
              </button>
              <button
                className="btn"
                style={{ background: currentView === 'setup' ? 'var(--bg-subtle)' : 'transparent', border: 'none' }}
                onClick={() => setCurrentView('setup')}
              >
                <Settings size={16} /> 设置
              </button>
            </nav>
          </div>

          <button
            className="btn btn-demo"
            onClick={async () => {
              try {
                const res = await api.triggerDemo();
                handleDemoStarted(res.diagnosis_id);
              } catch (e: any) {
                alert('Demo 错误：' + e.message);
              }
            }}
          >
            <Sparkles size={14} /> 试用 Demo
          </button>
          <button className="btn" aria-label="切换主题" onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}>{theme === 'dark' ? <Sun size={16} /> : <Moon size={16} />}</button>
        </div>
      </header>

      {/* Main Content Area */}
      <main className="container" style={{ flex: 1, padding: '2rem 1.5rem' }}>
        {currentView === 'setup' && (
          <SetupPage
            onDemoStarted={handleDemoStarted}
            onConfigSaved={() => setCurrentView('repos')}
          />
        )}

        {currentView === 'repos' && (
          <RepositoriesPage
            onSelectRepoForDiagnosis={handleSelectRepoForDiagnosis}
          />
        )}

        {currentView === 'codeintel' && (
          <CodeIntelPage />
        )}

        {currentView === 'new-diag' && (
          <NewDiagnosisPage
            initialRepoId={preselectedRepoId}
            initialSnapshotId={preselectedSnapId}
            onDiagnosisCreated={handleDiagnosisCreated}
          />
        )}

        {currentView === 'diag-view' && activeDiagnosisId && (
          <DiagnosisView
            diagnosisId={activeDiagnosisId}
            onBack={() => setCurrentView('history')}
          />
        )}

        {currentView === 'history' && (
          <div>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.5rem' }}>
              <div>
                <h1 style={{ fontSize: '1.75rem', fontWeight: 700, color: 'var(--text-bright)' }}>诊断历史</h1>
                <p style={{ color: 'var(--text-muted)', fontSize: '0.9rem' }}>
                  按时间查看代码智能任务与有依据的诊断报告。
                </p>
              </div>
              <button className="btn btn-primary" onClick={() => setCurrentView('new-diag')}>
                <PlusCircle size={16} /> 开始新诊断
              </button>
            </div>

            {recentDiagnoses.length === 0 ? (
              <div className="card" style={{ textAlign: 'center', padding: '3rem 1rem' }}>
                <Activity size={36} color="var(--text-muted)" style={{ margin: '0 auto 1rem' }} />
                <h3 style={{ fontSize: '1.1rem', color: 'var(--text-bright)', marginBottom: '0.5rem' }}>暂无诊断记录</h3>
                <p style={{ color: 'var(--text-muted)', fontSize: '0.875rem', marginBottom: '1.5rem' }}>
                  请先对已索引仓库发起诊断，或点击 <strong>试用 Demo</strong>。
                </p>
              </div>
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
                {recentDiagnoses.map((d) => (
                  <div
                    key={d.id}
                    className="card"
                    style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', cursor: 'pointer' }}
                    onClick={() => {
                      setActiveDiagnosisId(d.id);
                      setCurrentView('diag-view');
                    }}
                  >
                    <div>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.25rem' }}>
                        <h3 style={{ fontSize: '1.05rem', fontWeight: 600, color: 'var(--text-bright)' }}>{d.issue_title}</h3>
                        <span className={`badge ${d.status === 'SUCCEEDED' ? 'badge-success' : d.status === 'FAILED' ? 'badge-danger' : 'badge-warning'}`}>
                          {{ SUCCEEDED: '成功', FAILED: '失败', CANCELLED: '已取消', RUNNING: '运行中', QUEUED: '排队中' }[d.status] || d.status}
                        </span>
                      </div>
                      <div style={{ fontSize: '0.8rem', color: 'var(--text-muted)' }}>
                        仓库：{d.repository_id}｜创建于：{new Date(d.created_at).toLocaleString('zh-CN')}
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </main>

      {/* Footer */}
      <footer style={{ borderTop: '1px solid var(--border-color)', padding: '1rem 0', color: 'var(--text-muted)', fontSize: '0.8rem', textAlign: 'center' }}>
        <div className="container">
          RepoLens v2.1 本地代码智能与根因分析引擎 · 本地零泄漏存储
        </div>
      </footer>
    </div>
  );
};
