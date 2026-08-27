import React from 'react';
import { AgentStep } from '../types';
import { Clock, Cpu } from 'lucide-react';

interface Props {
  steps: AgentStep[];
}

export const TraceViewer: React.FC<Props> = ({ steps }) => {
  if (!steps || steps.length === 0) {
    return (
      <div className="card" style={{ color: 'var(--text-muted)', fontSize: '0.875rem' }}>
        此次执行暂无 Agent 运行轨迹。
      </div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      {steps.map((s) => (
        <div key={s.id} className="card" style={{ padding: '0.85rem 1rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '0.35rem' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
              <span className="badge badge-purple" style={{ fontSize: '0.7rem' }}>步骤 #{s.seq}</span>
              <span style={{ fontFamily: 'var(--font-mono)', fontWeight: 600, color: 'var(--accent-primary)', fontSize: '0.875rem' }}>
                {s.tool_name ? `工具：${s.tool_name}` : s.step_type}
              </span>
            </div>

            <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem', color: 'var(--text-muted)', fontSize: '0.75rem' }}>
              <span style={{ display: 'flex', alignItems: 'center', gap: '0.2rem' }}>
                <Clock size={12} /> {s.latency_ms}ms
              </span>
              <span style={{ display: 'flex', alignItems: 'center', gap: '0.2rem' }}>
                <Cpu size={12} /> {s.input_tokens + s.output_tokens} tok
              </span>
              <span className="badge badge-success" style={{ fontSize: '0.65rem' }}>{s.status}</span>
            </div>
          </div>

          {s.tool_args_summary && (
            <div style={{ fontSize: '0.8rem', color: 'var(--text-muted)', marginTop: '0.25rem', fontFamily: 'var(--font-mono)' }}>
              <strong>参数：</strong>
              <span>{s.tool_args_summary}</span>
            </div>
          )}

          {s.tool_result_summary && (
            <div style={{ fontSize: '0.8rem', color: 'var(--text-bright)', marginTop: '0.25rem', fontFamily: 'var(--font-mono)' }}>
              <strong>结果：</strong>
              <span>{s.tool_result_summary}</span>
            </div>
          )}
        </div>
      ))}
    </div>
  );
};
