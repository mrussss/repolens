import React, { useState, useEffect } from 'react';
import { SetupPage } from './components/SetupPage';
import { RepositoriesPage } from './components/RepositoriesPage';
import { NewDiagnosisPage } from './components/NewDiagnosisPage';
import { DiagnosisView } from './components/DiagnosisView';
import { api } from './api';
import { DiagnosisRun } from './types';
import { FolderGit2, PlusCircle, Settings, Sparkles, Activity, Clock } from 'lucide-react';

type ViewMode = 'setup' | 'repos' | 'new-diag' | 'diag-view' | 'history';

export const App: React.FC = () => {
  const [currentView, setCurrentView] = useState<ViewMode>('setup');
  const [activeDiagnosisId, setActiveDiagnosisId] = useState<string | null>(null);
  const [preselectedRepoId, setPreselectedRepoId] = useState<string>('');
  const [preselectedSnapId, setPreselectedSnapId] = useState<string>('');
  const [recentDiagnoses, setRecentDiagnoses] = useState<DiagnosisRun[]>([]);

  useEffect(() => {
    loadRecentDiagnoses();
  }, [currentView]);

  const loadRecentDiagnoses = async () => {
    try {
      const list = await api.listDiagnoses();
      setRecentDiagnoses(list || []);
      // If user has existing diagnoses and currently on initial load, show history or repos
      if (list && list.length > 0 && currentView === 'setup') {
        const isConfigured = (await api.getProviderStatus()).is_configured;
        if (isConfigured) {
          // keep setup or allow user to navigate
        }
      }
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
                <FolderGit2 size={16} /> Repositories
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
                <PlusCircle size={16} /> New Diagnosis
              </button>
              <button
                className="btn"
                style={{ background: currentView === 'history' ? 'var(--bg-subtle)' : 'transparent', border: 'none' }}
                onClick={() => setCurrentView('history')}
              >
                <Clock size={16} /> History ({recentDiagnoses.length})
              </button>
              <button
                className="btn"
                style={{ background: currentView === 'setup' ? 'var(--bg-subtle)' : 'transparent', border: 'none' }}
                onClick={() => setCurrentView('setup')}
              >
                <Settings size={16} /> Setup
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
                alert('Demo error: ' + e.message);
              }
            }}
          >
            <Sparkles size={14} /> Try Demo
          </button>
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
                <h1 style={{ fontSize: '1.75rem', fontWeight: 700, color: 'var(--text-bright)' }}>Diagnosis History</h1>
                <p style={{ color: 'var(--text-muted)', fontSize: '0.9rem' }}>
                  Chronological record of code intelligence runs and grounded reports.
                </p>
              </div>
              <button className="btn btn-primary" onClick={() => setCurrentView('new-diag')}>
                <PlusCircle size={16} /> Start New Diagnosis
              </button>
            </div>

            {recentDiagnoses.length === 0 ? (
              <div className="card" style={{ textAlign: 'center', padding: '3rem 1rem' }}>
                <Activity size={36} color="var(--text-muted)" style={{ margin: '0 auto 1rem' }} />
                <h3 style={{ fontSize: '1.1rem', color: 'var(--text-bright)', marginBottom: '0.5rem' }}>No Diagnoses Yet</h3>
                <p style={{ color: 'var(--text-muted)', fontSize: '0.875rem', marginBottom: '1.5rem' }}>
                  Run your first diagnosis on an indexed repository or click <strong>Try Demo</strong>.
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
                          {d.status}
                        </span>
                      </div>
                      <div style={{ fontSize: '0.8rem', color: 'var(--text-muted)' }}>
                        Repo ID: {d.repository_id} | Created: {new Date(d.created_at).toLocaleString()}
                      </div>
                    </div>

                    <button className="btn" style={{ fontSize: '0.8rem' }}>
                      View Report →
                    </button>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </main>

      {/* Footer */}
      <footer style={{ borderTop: '1px solid var(--border-color)', background: 'var(--bg-card)', padding: '1rem 0', color: 'var(--text-muted)', fontSize: '0.8rem', textAlign: 'center' }}>
        <div className="container">
          RepoLens v2.1 — Grounded Code Intelligence & Automated Root Cause Analysis
        </div>
      </footer>
    </div>
  );
};
