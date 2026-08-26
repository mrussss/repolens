import React, { useState, useEffect } from 'react';
import { api } from '../api';
import { Repository, Snapshot, CodeIndexBuild, CodeSymbol, QualityReport, SymbolRelation, RetrievalBuild } from '../types';
import { Search, CheckCircle2, RefreshCw, BarChart2, Layers } from 'lucide-react';

export const CodeIntelPage: React.FC = () => {
  const [repos, setRepos] = useState<Repository[]>([]);
  const [selectedRepoId, setSelectedRepoId] = useState<string>('');
  const [selectedSnapshotId, setSelectedSnapshotId] = useState<string>('');
  const [buildId, setBuildId] = useState<number>(0);
  const [retrievalBuild, setRetrievalBuild] = useState<RetrievalBuild | null>(null);
  const [quality, setQuality] = useState<QualityReport | null>(null);
  const [symbols, setSymbols] = useState<CodeSymbol[]>([]);
  const [searchQuery, setSearchQuery] = useState<string>('');
  const [selectedSymbol, setSelectedSymbol] = useState<CodeSymbol | null>(null);
  const [references, setReferences] = useState<SymbolRelation[]>([]);
  const [relatedTests, setRelatedTests] = useState<SymbolRelation[]>([]);
  const [loading, setLoading] = useState(false);
  const [symbolLoading, setSymbolLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    loadInitialData();
  }, []);

  const loadInitialData = async () => {
    try {
      setLoading(true);
      const list = await api.listRepositories();
      setRepos(list || []);
      if (list && list.length > 0) {
        setSelectedRepoId(list[0].id);
        const snapshot = readySnapshots(list[0])[0];
        if (snapshot) prepareBuild(snapshot);
      }
    } catch {
      // ignore
    } finally {
      setLoading(false);
    }
  };

  const readySnapshots = (repo: Repository): Snapshot[] =>
    (repo.snapshots || []).filter((snapshot) => snapshot.status === 'READY');

  const waitForCodeIndex = async (id: number): Promise<CodeIndexBuild> => {
    for (let attempt = 0; attempt < 60; attempt += 1) {
      const build = await api.getCodeIndexBuild(id);
      if (build.status === 'READY' || build.status === 'FAILED') return build;
      await new Promise((resolve) => setTimeout(resolve, 1000));
    }
    throw new Error('Code index build is still running; refresh to continue.');
  };

  const waitForRetrieval = async (id: number): Promise<RetrievalBuild> => {
    for (let attempt = 0; attempt < 60; attempt += 1) {
      const build = await api.getRetrievalBuild(id);
      if (build.status === 'READY' || build.status === 'FAILED') return build;
      await new Promise((resolve) => setTimeout(resolve, 1000));
    }
    throw new Error('Retrieval build is still running; refresh to continue.');
  };

  const prepareBuild = async (snapshot: Snapshot) => {
    try {
      setLoading(true);
      setSelectedSnapshotId(snapshot.id);
      setQuality(null);
      setSymbols([]);
      setSelectedSymbol(null);
      const response = await api.triggerCodeIndexBuild(snapshot.id);
      const build = response.code_index_build.status === 'READY'
        ? response.code_index_build
        : await waitForCodeIndex(response.code_index_build.id);
      if (build.status !== 'READY') throw new Error(`Code index build ${build.status}`);
      setBuildId(build.id);

      const retrievalResponse = await api.triggerRetrievalBuild(build.id);
      const retrieval = retrievalResponse.retrieval_build.status === 'READY'
        ? retrievalResponse.retrieval_build
        : await waitForRetrieval(retrievalResponse.retrieval_build.id);
      if (retrieval.status !== 'READY') throw new Error(`Retrieval build ${retrieval.status}`);
      setRetrievalBuild(retrieval);
      await loadBuildDetails(build.id);
    } catch (err: any) {
      setError(err.message || 'Failed preparing code intelligence builds');
    } finally {
      setLoading(false);
    }
  };

  const loadBuildDetails = async (id: number) => {
    try {
      setLoading(true);
      setBuildId(id);
      const q = await api.getBuildQuality(id);
      setQuality(q);

      const symRes = await api.listSymbols(id, searchQuery);
      setSymbols(symRes.symbols || []);
      if (symRes.symbols && symRes.symbols.length > 0) {
        handleSelectSymbol(symRes.symbols[0], id);
      }
    } catch {
      // ignore
    } finally {
      setLoading(false);
    }
  };

  const handleSearch = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      setLoading(true);
      const symRes = await api.listSymbols(buildId, searchQuery);
      setSymbols(symRes.symbols || []);
      if (symRes.symbols && symRes.symbols.length > 0) {
        handleSelectSymbol(symRes.symbols[0], buildId);
      }
    } catch {
      // ignore
    } finally {
      setLoading(false);
    }
  };

  const handleSelectSymbol = async (sym: CodeSymbol, bId: number) => {
    setSelectedSymbol(sym);
    setSymbolLoading(true);
    try {
      const refRes = await api.getSymbolReferences(sym.id, bId);
      setReferences(refRes.relations || []);

      const testRes = await api.getSymbolRelatedTests(sym.id, bId, sym.symbol_key_hash);
      setRelatedTests(testRes.related_tests || []);
    } catch {
      // ignore
    } finally {
      setSymbolLoading(false);
    }
  };

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.5rem' }}>
        <div>
          <h1 style={{ fontSize: '1.75rem', fontWeight: 700, color: 'var(--text-bright)' }}>Code Intelligence Explorer</h1>
          <p style={{ color: 'var(--text-muted)', fontSize: '0.9rem' }}>
            Versioned AST Symbols, Canonical Receivers, Cross-package References, and Related Tests.
          </p>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <select
            className="input-field"
            style={{ width: 220 }}
            value={selectedRepoId}
            onChange={(e) => {
              setSelectedRepoId(e.target.value);
              const selectedRepo = repos.find((repo) => repo.id === e.target.value);
              const snapshot = selectedRepo ? readySnapshots(selectedRepo)[0] : undefined;
              if (snapshot) prepareBuild(snapshot);
            }}
          >
            {repos.map((r) => (
              <option key={r.id} value={r.id}>{r.name}</option>
            ))}
          </select>
          <select
            className="input-field"
            style={{ width: 260 }}
            value={selectedSnapshotId}
            onChange={(e) => {
              const snapshot = (repos.find((repo) => repo.id === selectedRepoId)?.snapshots || []).find((item) => item.id === e.target.value);
              if (snapshot) prepareBuild(snapshot);
            }}
            disabled={!selectedRepoId}
          >
            <option value="">Select READY snapshot</option>
            {(repos.find((repo) => repo.id === selectedRepoId) ? readySnapshots(repos.find((repo) => repo.id === selectedRepoId)!) : []).map((snapshot) => (
              <option key={snapshot.id} value={snapshot.id}>{snapshot.id} ({snapshot.commit_sha.slice(0, 12)})</option>
            ))}
          </select>
        </div>
      </div>

      {error && <div className="card" style={{ marginBottom: '1rem', color: 'var(--accent-danger)' }}>{error}</div>}

      {/* Quality Breakdown Metrics Banner */}
      {quality && (
        <div className="card" style={{ marginBottom: '1.5rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '1rem' }}>
            <BarChart2 size={18} color="var(--accent-primary)" />
            <h2 style={{ fontSize: '1.1rem', fontWeight: 600, color: 'var(--text-bright)' }}>
              Analysis Completeness & Certainty Distribution
            </h2>
            <span className="badge badge-success" style={{ marginLeft: 'auto' }}>
              Build #{quality.code_index_build_id} {quality.status}{retrievalBuild ? ` · Retrieval #${retrievalBuild.id} READY` : ''}
            </span>
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: '1rem', textAlign: 'center' }}>
            <div style={{ background: 'var(--bg-main)', padding: '0.75rem', borderRadius: 6, border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '1.4rem', fontWeight: 700, color: 'var(--text-bright)' }}>{quality.parsed_pct}</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Files Parsed ({quality.files_parsed}/{quality.files_total})</div>
            </div>
            <div style={{ background: 'var(--bg-main)', padding: '0.75rem', borderRadius: 6, border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '1.4rem', fontWeight: 700, color: 'var(--accent-primary)' }}>{quality.typechecked_pct}</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Typechecked ({quality.packages_typechecked}/{quality.packages_total})</div>
            </div>
            <div style={{ background: 'var(--bg-main)', padding: '0.75rem', borderRadius: 6, border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '1.4rem', fontWeight: 700, color: 'var(--accent-success)' }}>{quality.symbol_count}</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Authoritative Symbols</div>
            </div>
            <div style={{ background: 'var(--bg-main)', padding: '0.75rem', borderRadius: 6, border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '1.4rem', fontWeight: 700, color: 'var(--accent-purple)' }}>
                {quality.semantic_relation_count + quality.syntactic_relation_count}
              </div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
                Resolved Relations ({quality.semantic_relation_count} Semantic, {quality.syntactic_relation_count} Syntactic)
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Main 2-Column Code Intelligence Explorer */}
      <div style={{ display: 'grid', gridTemplateColumns: '400px 1fr', gap: '1.5rem', alignItems: 'start' }}>
        {/* Left Column: Symbol Search & List */}
        <div className="card">
          <form onSubmit={handleSearch} style={{ display: 'flex', gap: '0.5rem', marginBottom: '1rem' }}>
            <input
              type="text"
              className="input-field"
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              placeholder="Search symbols (e.g. ProcessOrder, Service)..."
            />
            <button type="submit" className="btn btn-primary">
              <Search size={14} />
            </button>
          </form>

          {loading ? (
            <div style={{ textAlign: 'center', padding: '2rem', color: 'var(--text-muted)' }}>
              <RefreshCw size={20} className="spin" />
            </div>
          ) : symbols.length === 0 ? (
            <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem', textAlign: 'center', padding: '2rem 0' }}>
              No symbols found in this build.
            </p>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', maxHeight: 550, overflowY: 'auto' }}>
              {symbols.map((sym) => {
                const isSelected = selectedSymbol?.id === sym.id;
                return (
                  <div
                    key={sym.id}
                    onClick={() => handleSelectSymbol(sym, buildId)}
                    style={{
                      padding: '0.65rem 0.75rem',
                      borderRadius: 6,
                      background: isSelected ? 'var(--bg-subtle)' : 'var(--bg-main)',
                      border: `1px solid ${isSelected ? 'var(--accent-primary)' : 'var(--border-color)'}`,
                      cursor: 'pointer',
                      transition: 'all 0.15s ease',
                    }}
                  >
                    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '0.2rem' }}>
                      <span style={{ fontWeight: 600, color: 'var(--text-bright)', fontSize: '0.9rem', fontFamily: 'var(--font-mono)' }}>
                        {sym.receiver_canonical ? `(${sym.receiver_canonical}).${sym.name}` : sym.name}
                      </span>
                      <span className="badge badge-info" style={{ fontSize: '0.65rem' }}>{sym.kind}</span>
                    </div>
                    <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
                      {sym.file_path}:L{sym.start_line}
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>

        {/* Right Column: Symbol Inspector */}
        <div className="card">
          {symbolLoading ? (
            <div style={{ textAlign: 'center', padding: '4rem 0', color: 'var(--text-muted)' }}>
              <RefreshCw size={24} className="spin" />
            </div>
          ) : selectedSymbol ? (
            <div>
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', borderBottom: '1px solid var(--border-color)', paddingBottom: '0.75rem', marginBottom: '1rem' }}>
                <div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                    <h2 style={{ fontSize: '1.25rem', fontWeight: 700, color: 'var(--text-bright)', fontFamily: 'var(--font-mono)' }}>
                      {selectedSymbol.receiver_canonical ? `(${selectedSymbol.receiver_canonical}).${selectedSymbol.name}` : selectedSymbol.name}
                    </h2>
                    <span className="badge badge-purple">{selectedSymbol.kind}</span>
                  </div>
                  <div style={{ fontSize: '0.8rem', color: 'var(--text-muted)', marginTop: '0.25rem' }}>
                    Package: <code style={{ color: 'var(--accent-primary)' }}>{selectedSymbol.package_path}</code> | File: <code>{selectedSymbol.file_path}:L{selectedSymbol.start_line}-L{selectedSymbol.end_line}</code>
                  </div>
                </div>
              </div>

              {/* Signature & Raw Key */}
              <div style={{ marginBottom: '1.25rem' }}>
                <h4 style={{ fontSize: '0.825rem', color: 'var(--text-muted)', marginBottom: '0.35rem' }}>Full Signature:</h4>
                <pre className="code-snippet">
                  <code>{selectedSymbol.signature || `${selectedSymbol.kind} ${selectedSymbol.name}()`}</code>
                </pre>
              </div>

              {/* Canonical Identity Key */}
              <div style={{ marginBottom: '1.25rem', background: 'var(--bg-main)', padding: '0.75rem', borderRadius: 6, border: '1px solid var(--border-color)' }}>
                <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
                  <strong>Symbol Key Raw: </strong>
                  <code>{selectedSymbol.symbol_key_raw}</code>
                </div>
                <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)', marginTop: '0.25rem' }}>
                  <strong>Key Hash: </strong>
                  <code>{selectedSymbol.symbol_key_hash}</code>
                </div>
              </div>

              {/* References & Callers */}
              <div style={{ marginBottom: '1.5rem' }}>
                <h3 style={{ fontSize: '1rem', fontWeight: 600, color: 'var(--text-bright)', marginBottom: '0.5rem', display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                  <Layers size={16} /> References & Callers ({references.length})
                </h3>
                {references.length === 0 ? (
                  <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem' }}>No incoming references or callers recorded.</p>
                ) : (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                    {references.map((r) => (
                      <div key={r.id} style={{ background: 'var(--bg-main)', padding: '0.6rem 0.75rem', borderRadius: 6, border: '1px solid var(--border-color)', fontSize: '0.825rem' }}>
                        <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.2rem' }}>
                          <span className="badge badge-info" style={{ fontSize: '0.65rem' }}>{r.relation_type} ({r.resolution_kind})</span>
                          <span style={{ color: 'var(--text-muted)', fontSize: '0.75rem' }}>{r.file_path}:L{r.line}</span>
                        </div>
                        <div style={{ fontFamily: 'var(--font-mono)', color: 'var(--text-bright)' }}>
                          Reason: {r.reason_code} {r.target_name ? `→ ${r.target_name}` : ''}
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </div>

              {/* Related Tests */}
              <div>
                <h3 style={{ fontSize: '1rem', fontWeight: 600, color: 'var(--text-bright)', marginBottom: '0.5rem', display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                  <CheckCircle2 size={16} color="var(--accent-success)" /> Related Tests ({relatedTests.length})
                </h3>
                {relatedTests.length === 0 ? (
                  <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem' }}>No related test discovered for this symbol.</p>
                ) : (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                    {relatedTests.map((t) => (
                      <div key={t.id} style={{ background: 'var(--bg-main)', padding: '0.6rem 0.75rem', borderRadius: 6, border: '1px solid rgba(63,185,80,0.3)', fontSize: '0.825rem' }}>
                        <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.2rem' }}>
                          <span className="badge badge-success" style={{ fontSize: '0.65rem' }}>{t.reason_code}</span>
                          <span style={{ color: 'var(--text-muted)', fontSize: '0.75rem' }}>{t.file_path}:L{t.line}</span>
                        </div>
                        <div style={{ fontFamily: 'var(--font-mono)', color: 'var(--accent-success)' }}>
                          {t.target_name || 'Test function'}
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </div>
          ) : (
            <p style={{ color: 'var(--text-muted)', fontSize: '0.85rem', textAlign: 'center', padding: '4rem 0' }}>
              Select a symbol on the left to inspect its signature, AST properties, references, and related tests.
            </p>
          )}
        </div>
      </div>
    </div>
  );
};
