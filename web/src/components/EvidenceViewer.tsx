import React, { useState } from 'react';
import { Finding } from '../types';
import { FileCode, ChevronDown, ChevronRight, CheckCircle2 } from 'lucide-react';

interface Props {
  findings: Finding[];
}

export const EvidenceViewer: React.FC<Props> = ({ findings }) => {
  const [expandedIndices, setExpandedIndices] = useState<Record<number, boolean>>({ 0: true });

  const toggleExpand = (index: number) => {
    setExpandedIndices((prev) => ({ ...prev, [index]: !prev[index] }));
  };

  if (!findings || findings.length === 0) {
    return (
      <div className="card" style={{ color: 'var(--text-muted)', fontSize: '0.875rem' }}>
        暂无有依据的代码证据。
      </div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {findings.map((f, i) => {
        const isExpanded = !!expandedIndices[i];
        return (
          <div key={i} className="card" style={{ borderLeft: '4px solid var(--accent-primary)' }}>
            <div
              style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', cursor: 'pointer' }}
              onClick={() => toggleExpand(i)}
            >
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <CheckCircle2 size={18} color="var(--accent-success)" />
                <h3 style={{ fontSize: '1.05rem', fontWeight: 600, color: 'var(--text-bright)' }}>{f.title}</h3>
              </div>
              <button className="btn" style={{ padding: '0.25rem 0.5rem', fontSize: '0.75rem' }}>
                {isExpanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                {f.citations?.length || 0} 条引用
              </button>
            </div>

            <p style={{ marginTop: '0.75rem', fontSize: '0.9rem', color: 'var(--text-main)' }}>
              {f.reasoning}
            </p>

            {isExpanded && f.citations && f.citations.length > 0 && (
              <div style={{ marginTop: '1rem', display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
                {f.citations.map((c, ci) => (
                  <div key={ci} style={{ background: 'var(--bg-main)', border: '1px solid var(--border-color)', borderRadius: 6, padding: '0.75rem' }}>
                    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '0.35rem', color: 'var(--accent-primary)', fontSize: '0.825rem', fontFamily: 'var(--font-mono)' }}>
                        <FileCode size={14} />
                        <strong>{c.file_path}</strong>
                        <span style={{ color: 'var(--text-muted)' }}>:L{c.start_line}-L{c.end_line}</span>
                      </div>
                      {c.reason && (
                        <span className="badge badge-info" style={{ fontSize: '0.7rem' }}>{c.reason}</span>
                      )}
                    </div>
                    <pre className="code-snippet">
                      <code>{c.excerpt}</code>
                    </pre>
                  </div>
                ))}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
};
