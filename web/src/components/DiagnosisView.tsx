import React, { useState, useEffect } from 'react';
import { api } from '../api';
import { DiagnosisRun, DiagnosisReport, AgentStep } from '../types';
import { EvidenceViewer } from './EvidenceViewer';
import { TraceViewer } from './TraceViewer';
import { RefreshCw, StopCircle, FileText, Activity, ShieldAlert, ArrowLeft } from 'lucide-react';

interface Props {
  diagnosisId: string;
  onBack: () => void;
}

export const DiagnosisView: React.FC<Props> = ({ diagnosisId, onBack }) => {
  const [run, setRun] = useState<DiagnosisRun | null>(null);
  const [report, setReport] = useState<DiagnosisReport | null>(null);
  const [steps, setSteps] = useState<AgentStep[]>([]);
  const [activeTab, setActiveTab] = useState<'evidence' | 'trace'>('evidence');
  const [loading, setLoading] = useState(true);
  const [cancelling, setCancelling] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let interval: any = null;

    const fetchAll = async () => {
      try {
        const r = await api.getDiagnosis(diagnosisId);
        setRun(r);

        if (r.status === 'SUCCEEDED') {
          try {
            const rep = await api.getDiagnosisReport(diagnosisId);
            setReport(rep);
          } catch {}
          try {
            const st = await api.getDiagnosisSteps(diagnosisId);
            setSteps(st || []);
          } catch {}
        } else if (r.status === 'RUNNING' || r.status === 'QUEUED') {
          try {
            const st = await api.getDiagnosisSteps(diagnosisId);
            setSteps(st || []);
          } catch {}
        }
      } catch (err: any) {
        setError(err.message || 'Failed polling diagnosis run');
      } finally {
        setLoading(false);
      }
    };

    fetchAll();

    // Poll every 1.5s while active
    interval = setInterval(() => {
      if (!run || run.status === 'QUEUED' || run.status === 'RUNNING') {
        fetchAll();
      }
    }, 1500);

    return () => clearInterval(interval);
  }, [diagnosisId, run?.status]);

  const handleCancel = async () => {
    setCancelling(true);
    try {
      await api.cancelDiagnosis(diagnosisId);
      const updated = await api.getDiagnosis(diagnosisId);
      setRun(updated);
    } catch (err: any) {
      setError(err.message || 'Failed cancelling diagnosis');
    } finally {
      setCancelling(false);
    }
  };

  const getStatusBadge = (status: string) => {
    switch (status) {
      case 'SUCCEEDED': return <span className="badge badge-success">SUCCEEDED</span>;
      case 'RUNNING': return <span className="badge badge-info"><RefreshCw size={12} className="spin" style={{ marginRight: 4 }} /> RUNNING</span>;
      case 'QUEUED': return <span className="badge badge-warning">QUEUED</span>;
      case 'FAILED': return <span className="badge badge-danger">FAILED</span>;
      case 'CANCELLED': return <span className="badge badge-danger">CANCELLED</span>;
      default: return <span className="badge">{status}</span>;
    }
  };

  if (loading && !run) {
    return (
      <div style={{ textAlign: 'center', padding: '4rem', color: 'var(--text-muted)' }}>
        <RefreshCw size={28} className="spin" />
        <p style={{ marginTop: '0.75rem' }}>Loading diagnosis run...</p>
      </div>
    );
  }

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.5rem' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <button className="btn" onClick={onBack}>
            <ArrowLeft size={16} /> Back
          </button>
          <div>
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
              <h1 style={{ fontSize: '1.5rem', fontWeight: 700, color: 'var(--text-bright)' }}>
                {run?.issue_title}
              </h1>
              {run && getStatusBadge(run.status)}
            </div>
            <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem' }}>
              Diagnosis ID: <code style={{ color: 'var(--text-bright)' }}>{run?.id}</code>
            </p>
          </div>
        </div>

        {(run?.status === 'QUEUED' || run?.status === 'RUNNING') && (
          <button className="btn btn-danger" onClick={handleCancel} disabled={cancelling}>
            <StopCircle size={16} /> {cancelling ? 'Cancelling...' : 'Cancel Run'}
          </button>
        )}
      </div>

      {error && (
        <div style={{ padding: '0.75rem', background: 'rgba(248,81,73,0.15)', border: '1px solid rgba(248,81,73,0.3)', borderRadius: 6, color: 'var(--accent-danger)', marginBottom: '1rem', fontSize: '0.85rem' }}>
          {error}
        </div>
      )}

      {/* In-Flight Status Progress Banner */}
      {(run?.status === 'QUEUED' || run?.status === 'RUNNING') && (
        <div className="card" style={{ background: '#1c2128', border: '1px solid var(--accent-primary)' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '1rem' }}>
            <RefreshCw size={24} className="spin" color="var(--accent-primary)" />
            <div>
              <h3 style={{ color: 'var(--text-bright)', fontSize: '1rem', fontWeight: 600 }}>
                {run.status === 'QUEUED' && 'Diagnosis Job Queued...'}
                {run.status === 'RUNNING' && 'Autonomous AI Agent Investigating Codebase...'}
              </h3>
              <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem' }}>
                Executing code search, reading AST symbols, and inspecting stack traces in real time.
              </p>
            </div>
          </div>
        </div>
      )}

      {/* Root Cause Card (if Succeeded) */}
      {report && (
        <div className="card" style={{ borderLeft: '4px solid var(--accent-success)', marginBottom: '1.5rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.5rem' }}>
            <ShieldAlert size={20} color="var(--accent-success)" />
            <h2 style={{ fontSize: '1.2rem', fontWeight: 700, color: 'var(--text-bright)' }}>Diagnosed Root Cause</h2>
            <span className="badge badge-success" style={{ marginLeft: 'auto' }}>
              Confidence: {Math.round(report.confidence * 100)}%
            </span>
          </div>
          <p style={{ fontSize: '0.95rem', color: 'var(--text-bright)', lineHeight: 1.6, marginTop: '0.5rem' }}>
            {report.root_cause}
          </p>

          {/* Recommended Checks */}
          {report.recommended_checks && report.recommended_checks.length > 0 && (
            <div style={{ marginTop: '1rem', paddingTop: '1rem', borderTop: '1px solid var(--border-color)' }}>
              <h4 style={{ fontSize: '0.875rem', fontWeight: 600, color: 'var(--text-bright)', marginBottom: '0.5rem' }}>
                Recommended Action Items & Verifications:
              </h4>
              <ul style={{ paddingLeft: '1.25rem', fontSize: '0.85rem', color: 'var(--text-main)', display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
                {report.recommended_checks.map((chk, i) => (
                  <li key={i}>{chk}</li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}

      {/* Navigation Tabs */}
      <div style={{ display: 'flex', gap: '0.5rem', borderBottom: '1px solid var(--border-color)', marginBottom: '1rem' }}>
        <button
          className="btn"
          style={{
            borderRadius: '6px 6px 0 0',
            borderBottom: activeTab === 'evidence' ? '2px solid var(--accent-primary)' : 'none',
            background: activeTab === 'evidence' ? 'var(--bg-card)' : 'transparent',
            border: 'none',
          }}
          onClick={() => setActiveTab('evidence')}
        >
          <FileText size={16} /> Grounded Evidence ({report?.findings?.length || 0})
        </button>
        <button
          className="btn"
          style={{
            borderRadius: '6px 6px 0 0',
            borderBottom: activeTab === 'trace' ? '2px solid var(--accent-primary)' : 'none',
            background: activeTab === 'trace' ? 'var(--bg-card)' : 'transparent',
            border: 'none',
          }}
          onClick={() => setActiveTab('trace')}
        >
          <Activity size={16} /> Agent Execution Trace ({steps.length})
        </button>
      </div>

      {activeTab === 'evidence' && <EvidenceViewer findings={report?.findings || []} />}
      {activeTab === 'trace' && <TraceViewer steps={steps} />}
    </div>
  );
};
