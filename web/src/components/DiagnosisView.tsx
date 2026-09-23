import React, { useState, useEffect } from 'react';
import { api } from '../api';
import { DiagnosisRun, DiagnosisReport, DiagnosisAttempt, AgentStep } from '../types';
import { EvidenceViewer } from './EvidenceViewer';
import { TraceViewer } from './TraceViewer';
import { isInvalidReport } from '../reportStatus';
import { RefreshCw, StopCircle, FileText, Activity, ShieldAlert, ArrowLeft } from 'lucide-react';

interface Props {
  diagnosisId: string;
  onBack: () => void;
}

export const DiagnosisView: React.FC<Props> = ({ diagnosisId, onBack }) => {
  const [run, setRun] = useState<DiagnosisRun | null>(null);
  const [report, setReport] = useState<DiagnosisReport | null>(null);
  const [steps, setSteps] = useState<AgentStep[]>([]);
  const [attempts, setAttempts] = useState<DiagnosisAttempt[]>([]);
  const [activeTab, setActiveTab] = useState<'evidence' | 'trace'>('evidence');
  const [loading, setLoading] = useState(true);
  const [cancelling, setCancelling] = useState(false);
  const [retrying, setRetrying] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pollEpoch, setPollEpoch] = useState(0);

  useEffect(() => {
    let stopped = false;
    let timer: number | undefined;
    let delay = 1000;

    setRun(null);
    setReport(null);
    setSteps([]);
    setAttempts([]);
    setError(null);
    setLoading(true);

    const fetchAll = async (): Promise<boolean> => {
      if (stopped) return false;
      let active = false;
      try {
        const r = await api.getDiagnosis(diagnosisId);
        if (stopped) return false;
        setRun(r);
        active = r.status === 'RUNNING' || r.status === 'QUEUED';

        try {
          const loadedAttempts = await api.getDiagnosisAttempts(diagnosisId);
          if (stopped) return false;
          setAttempts(loadedAttempts);
        } catch {}

        if (r.status === 'SUCCEEDED' || (r.status === 'FAILED' && !!r.final_attempt_id)) {
          try {
            const rep = await api.getDiagnosisReport(diagnosisId);
            if (stopped) return false;
            setReport(rep);
          } catch {}
          try {
            const st = await api.getDiagnosisSteps(diagnosisId);
            if (stopped) return false;
            setSteps(st || []);
          } catch {}
        }
        if (r.status === 'RUNNING' || r.status === 'QUEUED' || (!!r.final_attempt_id && r.status !== 'SUCCEEDED')) {
          try {
            const st = await api.getDiagnosisSteps(diagnosisId);
            if (stopped) return false;
            setSteps(st || []);
          } catch {}
        }
      } catch (err: any) {
        if (stopped) return false;
        setError(err.message || '刷新诊断状态失败');
      } finally {
        if (!stopped) setLoading(false);
      }
      return active;
    };

    const poll = async () => {
      if (stopped) return;
      if (document.hidden) {
        timer = window.setTimeout(poll, 5000);
        return;
      }
      const active = await fetchAll();
      if (stopped || !active) return;
      delay = Math.min(5000, delay * 2);
      timer = window.setTimeout(poll, delay);
    };
    const onVisibilityChange = () => {
      if (!document.hidden) {
        delay = 1000;
        if (timer !== undefined) window.clearTimeout(timer);
        void poll();
      }
    };

    document.addEventListener('visibilitychange', onVisibilityChange);
    void poll();

    return () => {
      stopped = true;
      if (timer !== undefined) window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisibilityChange);
    };
  }, [diagnosisId, pollEpoch]);

  const handleCancel = async () => {
    setCancelling(true);
    try {
      await api.cancelDiagnosis(diagnosisId);
      const updated = await api.getDiagnosis(diagnosisId);
      setRun(updated);
    } catch (err: any) {
      setError(err.message || '取消诊断失败');
    } finally {
      setCancelling(false);
    }
  };

  const handleRetry = async () => {
    setRetrying(true);
    try {
      await api.retryDiagnosis(diagnosisId);
      setReport(null);
      setSteps([]);
      setAttempts([]);
      setError(null);
      setRun(await api.getDiagnosis(diagnosisId));
      setPollEpoch((epoch) => epoch + 1);
    } catch (err: any) {
      setError(err.message || '重试诊断失败');
    } finally {
      setRetrying(false);
    }
  };

  const getStatusBadge = (status: string) => {
    switch (status) {
      case 'SUCCEEDED': return <span className="badge badge-success">成功</span>;
      case 'RUNNING': return <span className="badge badge-info"><RefreshCw size={12} className="spin" style={{ marginRight: 4 }} /> 运行中</span>;
      case 'QUEUED': return <span className="badge badge-warning">排队中</span>;
      case 'FAILED': return <span className="badge badge-danger">失败</span>;
      case 'CANCELLED': return <span className="badge badge-danger">已取消</span>;
      default: return <span className="badge">{status}</span>;
    }
  };

  if (loading && !run) {
    return (
      <div style={{ textAlign: 'center', padding: '4rem', color: 'var(--text-muted)' }}>
        <RefreshCw size={28} className="spin" />
        <p style={{ marginTop: '0.75rem' }}>正在加载诊断任务…</p>
      </div>
    );
  }

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.5rem' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <button className="btn" onClick={onBack}>
            <ArrowLeft size={16} /> 返回
          </button>
          <div>
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
              <h1 style={{ fontSize: '1.5rem', fontWeight: 700, color: 'var(--text-bright)' }}>
                {run?.issue_title}
              </h1>
              {run && getStatusBadge(run.status)}
            </div>
            <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem' }}>
              诊断 ID：<code style={{ color: 'var(--text-bright)' }}>{run?.id}</code>
            </p>
          </div>
        </div>

        {(run?.status === 'QUEUED' || run?.status === 'RUNNING') && (
          <button className="btn btn-danger" onClick={handleCancel} disabled={cancelling}>
            <StopCircle size={16} /> {cancelling ? '取消中…' : '取消任务'}
          </button>
        )}
        {run?.status === 'FAILED' && run.retry_allowed && (
          <button className="btn" onClick={handleRetry} disabled={retrying}>
            <RefreshCw size={16} /> {retrying ? '重试中…' : '重试诊断'}
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
        <div className="card" style={{ background: 'var(--bg-subtle)', border: '1px solid var(--accent-primary)' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '1rem' }}>
            <RefreshCw size={24} className="spin" color="var(--accent-primary)" />
            <div>
              <h3 style={{ color: 'var(--text-bright)', fontSize: '1rem', fontWeight: 600 }}>
                {run.status === 'QUEUED' && '诊断任务排队中…'}
                {run.status === 'RUNNING' && 'AI Agent 正在分析代码库…'}
              </h3>
              <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem' }}>
                正在执行代码搜索、读取 AST 符号并实时检查堆栈信息。
              </p>
            </div>
          </div>
        </div>
      )}

      {attempts.length > 0 && (() => {
        const attempt = attempts.find((candidate) => candidate.id === run?.final_attempt_id) || attempts[attempts.length - 1];
        return (
          <div className="card" style={{ marginBottom: '1.5rem', color: 'var(--text-muted)', fontSize: '0.85rem' }}>
            执行详情：Attempt {attempt.attempt_no} · {attempt.status} · rounds {attempt.agent_rounds || 0} · tools {attempt.tool_calls || 0} · search {attempt.search_calls || 0} · provider calls {attempt.provider_calls || 0} · tokens {(attempt.prompt_tokens || 0) + (attempt.completion_tokens || 0)}
            {attempt.error_code && <div style={{ color: 'var(--accent-warning)', marginTop: '0.35rem' }}>失败位置：{attempt.error_code}</div>}
          </div>
        );
      })()}

      {run?.status === 'FAILED' && !report && run.final_attempt_id && (
        <div className="card" style={{ marginBottom: '1.5rem', color: 'var(--text-muted)' }}>
          本次执行未生成可展示的诊断报告。{run.retry_reason ? ` ${run.retry_reason}。` : ''}
          {run.retry_error_code && <span> 错误代码：<code>{run.retry_error_code}</code></span>}
        </div>
      )}
      {run?.status === 'FAILED' && !run.retry_allowed && run.retry_reason && !run.final_attempt_id && (
        <div className="card" style={{ marginBottom: '1.5rem', color: 'var(--text-muted)' }}>{run.retry_reason}</div>
      )}

      {/* Root Cause Card (if Succeeded) */}
      {report && (
        <div className="card" style={{ borderLeft: `4px solid ${report.report_status === 'VALID' ? 'var(--accent-success)' : 'var(--accent-warning)'}`, marginBottom: '1.5rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.5rem' }}>
            <ShieldAlert size={20} color={report.report_status === 'VALID' ? 'var(--accent-success)' : 'var(--accent-warning)'} />
            <h2 style={{ fontSize: '1.2rem', fontWeight: 700, color: 'var(--text-bright)' }}>诊断根因</h2>
            <span className={`badge ${report.report_status === 'VALID' ? 'badge-success' : 'badge-warning'}`} style={{ marginLeft: 'auto' }}>
              报告：{report.report_status || 'UNKNOWN'}
            </span>
          </div>
          {report.summary && <p style={{ color: 'var(--text-muted)', lineHeight: 1.6 }}>{report.summary}</p>}
          {report.model_claimed_confidence !== undefined && (
            <p style={{ color: 'var(--text-muted)', fontSize: '0.8rem', marginTop: '0.5rem' }}>
              模型自评 confidence：{report.model_claimed_confidence.toFixed(2)}
            </p>
          )}
          {!isInvalidReport(report) && report.root_cause ? (
            <p style={{ fontSize: '0.95rem', color: 'var(--text-bright)', lineHeight: 1.6, marginTop: '0.5rem' }}>{report.root_cause}</p>
          ) : (
            <p style={{ fontSize: '0.95rem', color: 'var(--accent-warning)', lineHeight: 1.6, marginTop: '0.5rem' }}>
              {report.report_status === 'INSUFFICIENT_EVIDENCE'
                ? '当前证据不足，不能将推断展示为已确认根因。'
                : 'Provider 输出未通过结构校验，已保留原始输出供调试；当前不能展示为已确认根因。'}
            </p>
          )}
          {isInvalidReport(report) && report.parse_error && (
            <p style={{ color: 'var(--text-muted)', fontSize: '0.8rem', marginTop: '0.5rem' }}>
              结构校验类别：<code>{report.parse_error}</code>
            </p>
          )}

          {report.limitations && report.limitations.length > 0 && (
            <div style={{ marginTop: '0.75rem', color: 'var(--text-muted)', fontSize: '0.85rem' }}>
              限制：{report.limitations.join('；')}
            </div>
          )}

          {report.confirmed_facts && report.confirmed_facts.length > 0 && (
            <div style={{ marginTop: '0.75rem', color: 'var(--text-muted)', fontSize: '0.85rem' }}>
              已确认事实：{report.confirmed_facts.join('；')}
            </div>
          )}

          {/* Recommended Checks */}
          {report.recommended_checks && report.recommended_checks.length > 0 && (
            <div style={{ marginTop: '1rem', paddingTop: '1rem', borderTop: '1px solid var(--border-color)' }}>
              <h4 style={{ fontSize: '0.875rem', fontWeight: 600, color: 'var(--text-bright)', marginBottom: '0.5rem' }}>
                建议的处理与验证项：
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
          <FileText size={16} /> 有依据的证据（{report?.findings?.length || 0}）
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
          <Activity size={16} /> Agent 执行轨迹（{steps.length}）
        </button>
      </div>

      {activeTab === 'evidence' && <EvidenceViewer findings={report?.findings || []} />}
      {activeTab === 'trace' && <TraceViewer steps={steps} />}
    </div>
  );
};
