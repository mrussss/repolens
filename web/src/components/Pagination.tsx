import React from 'react';

interface Props { page: number; pageSize: number; total: number; loading?: boolean; onPage: (page: number) => void }
export const Pagination: React.FC<Props> = ({ page, pageSize, total, loading, onPage }) => (
  <nav aria-label="分页" style={{ display: 'flex', justifyContent: 'center', gap: '1rem', alignItems: 'center', margin: '1rem 0' }}>
    <button className="btn" disabled={loading || page <= 1} onClick={() => onPage(page - 1)}>上一页</button>
    <span>第 {page} / {Math.max(1, Math.ceil(total / pageSize))} 页 · 共 {total} 条</span>
    <button className="btn" disabled={loading || page * pageSize >= total} onClick={() => onPage(page + 1)}>下一页</button>
  </nav>
);
