import React, { useState, useEffect } from 'react';
import { api } from '../api';
import { ProviderStatus } from '../types';
import { CheckCircle, Play, Sparkles, RefreshCw, Key, ShieldCheck } from 'lucide-react';

interface Props {
  onDemoStarted: (diagnosisId: string) => void;
  onConfigSaved: () => void;
}

export const SetupPage: React.FC<Props> = ({ onDemoStarted, onConfigSaved }) => {
  const [status, setStatus] = useState<ProviderStatus | null>(null);
  const [baseURL, setBaseURL] = useState('https://api.openai.com/v1');
  const [model, setModel] = useState('gpt-4o');
  const [apiKey, setApiKey] = useState('');
  const [loading, setLoading] = useState(false);
  const [testing, setTesting] = useState(false);
  const [demoLoading, setDemoLoading] = useState(false);
  const [testResult, setTestResult] = useState<{ success: boolean; latency_ms?: number; message?: string } | null>(null);
  const [saveSuccess, setSaveSuccess] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    loadStatus();
  }, []);

  const loadStatus = async () => {
    try {
      setLoading(true);
      const s = await api.getProviderStatus();
      setStatus(s);
      if (s.base_url) setBaseURL(s.base_url);
      if (s.model) setModel(s.model);
    } catch (err: any) {
      setError(err.message || 'Failed loading provider status');
    } finally {
      setLoading(false);
    }
  };

  const handleTest = async () => {
    if (!apiKey) {
      setError('Please provide an API Key for testing');
      return;
    }
    setTesting(true);
    setTestResult(null);
    setError(null);
    try {
      const res = await api.testProviderConnection({ base_url: baseURL, model, api_key: apiKey });
      setTestResult(res);
    } catch (err: any) {
      setTestResult({ success: false, message: err.message });
    } finally {
      setTesting(false);
    }
  };

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!apiKey) {
      setError('API Key cannot be empty');
      return;
    }
    setLoading(true);
    setError(null);
    setSaveSuccess(false);
    try {
      const res = await api.saveProviderConfig({ base_url: baseURL, model, api_key: apiKey });
      setStatus(res.status);
      setSaveSuccess(true);
      setApiKey(''); // Clear secret from browser memory immediately
      onConfigSaved();
    } catch (err: any) {
      setError(err.message || 'Failed saving provider configuration');
    } finally {
      setLoading(false);
    }
  };

  const handleTriggerDemo = async () => {
    setDemoLoading(true);
    setError(null);
    try {
      const res = await api.triggerDemo();
      onDemoStarted(res.diagnosis_id);
    } catch (err: any) {
      setError(err.message || 'Failed initiating Demo environment');
    } finally {
      setDemoLoading(false);
    }
  };

  return (
    <div style={{ maxWidth: 800, margin: '0 auto' }}>
      <div style={{ marginBottom: '2rem', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <div>
          <h1 style={{ fontSize: '1.75rem', fontWeight: 700, color: 'var(--text-bright)' }}>System & LLM Setup</h1>
          <p style={{ color: 'var(--text-muted)', fontSize: '0.9rem' }}>
            Configure your OpenAI-compatible inference provider or explore with 1-Click Demo Mode.
          </p>
        </div>
        <button
          className="btn btn-demo"
          onClick={handleTriggerDemo}
          disabled={demoLoading}
          style={{ padding: '0.65rem 1.25rem', fontSize: '0.95rem' }}
        >
          {demoLoading ? <RefreshCw size={16} className="spin" /> : <Sparkles size={16} />}
          Try Demo (No API Key Required)
        </button>
      </div>

      {/* Current Status Card */}
      <div className="card">
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '0.75rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
            <ShieldCheck size={20} color={status?.is_configured ? 'var(--accent-success)' : 'var(--text-muted)'} />
            <h2 style={{ fontSize: '1.1rem', fontWeight: 600, color: 'var(--text-bright)' }}>Active Provider Status</h2>
          </div>
          {status?.is_configured ? (
            <span className="badge badge-success">Configured</span>
          ) : (
            <span className="badge badge-warning">Unconfigured</span>
          )}
        </div>

        {status?.is_configured ? (
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1rem', marginTop: '1rem', fontSize: '0.85rem' }}>
            <div>
              <span style={{ color: 'var(--text-muted)' }}>Base URL: </span>
              <code style={{ color: 'var(--text-bright)' }}>{status.base_url}</code>
            </div>
            <div>
              <span style={{ color: 'var(--text-muted)' }}>Model: </span>
              <code style={{ color: 'var(--accent-primary)' }}>{status.model}</code>
            </div>
            <div>
              <span style={{ color: 'var(--text-muted)' }}>Endpoint Fingerprint: </span>
              <code style={{ fontSize: '0.75rem' }}>{status.endpoint_fingerprint?.slice(0, 16)}...</code>
            </div>
            <div>
              <span style={{ color: 'var(--text-muted)' }}>Mode: </span>
              <span>{status.is_demo ? 'Deterministic Local Demo' : 'Live Inference'}</span>
            </div>
          </div>
        ) : (
          <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem' }}>
            No LLM provider is currently configured. Provide your OpenAI-compatible Base URL & API Key below, or click <strong>Try Demo</strong>.
          </p>
        )}
      </div>

      {/* Provider Config Form */}
      <div className="card">
        <h2 style={{ fontSize: '1.1rem', fontWeight: 600, color: 'var(--text-bright)', marginBottom: '1rem', display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
          <Key size={18} /> Configure Provider Credentials
        </h2>

        {error && (
          <div style={{ padding: '0.75rem', background: 'rgba(248,81,73,0.15)', border: '1px solid rgba(248,81,73,0.3)', borderRadius: 6, color: 'var(--accent-danger)', marginBottom: '1rem', fontSize: '0.85rem' }}>
            {error}
          </div>
        )}

        {saveSuccess && (
          <div style={{ padding: '0.75rem', background: 'rgba(63,185,80,0.15)', border: '1px solid rgba(63,185,80,0.3)', borderRadius: 6, color: 'var(--accent-success)', marginBottom: '1rem', fontSize: '0.85rem', display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
            <CheckCircle size={16} /> Configuration saved securely with 0600 file permissions!
          </div>
        )}

        {testResult && (
          <div style={{
            padding: '0.75rem',
            background: testResult.success ? 'rgba(63,185,80,0.15)' : 'rgba(248,81,73,0.15)',
            border: `1px solid ${testResult.success ? 'rgba(63,185,80,0.3)' : 'rgba(248,81,73,0.3)'}`,
            borderRadius: 6,
            color: testResult.success ? 'var(--accent-success)' : 'var(--accent-danger)',
            marginBottom: '1rem',
            fontSize: '0.85rem'
          }}>
            {testResult.success ? `✓ ${testResult.message} (${testResult.latency_ms}ms)` : `✗ ${testResult.message}`}
          </div>
        )}

        <form onSubmit={handleSave}>
          <div className="form-group">
            <label className="form-label">OpenAI-Compatible Base URL</label>
            <input
              type="text"
              className="input-field"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
              placeholder="https://api.openai.com/v1"
              required
            />
            <div className="form-hint">Normalized automatically (e.g. OpenAI, DeepSeek, vLLM, Ollama, OpenRouter).</div>
          </div>

          <div className="form-group">
            <label className="form-label">Model Name</label>
            <input
              type="text"
              className="input-field"
              value={model}
              onChange={(e) => setModel(e.target.value)}
              placeholder="gpt-4o, deepseek-chat, claude-3-5-sonnet..."
              required
            />
          </div>

          <div className="form-group">
            <label className="form-label">API Key</label>
            <input
              type="password"
              className="input-field"
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              placeholder="sk-..."
              required
            />
            <div className="form-hint">Never stored in browser or logs; written atomically with 0600 permissions.</div>
          </div>

          <div style={{ display: 'flex', gap: '0.75rem', marginTop: '1.5rem' }}>
            <button
              type="button"
              className="btn"
              onClick={handleTest}
              disabled={testing || !apiKey}
            >
              {testing ? <RefreshCw size={16} className="spin" /> : <Play size={16} />}
              Test Connection
            </button>
            <button
              type="submit"
              className="btn btn-primary"
              disabled={loading || !apiKey}
            >
              {loading ? 'Saving...' : 'Save Configuration'}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};
