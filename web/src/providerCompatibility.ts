import type { TestProviderConnectionResult } from './api';

export interface ProviderTestAlert {
  className: 'alert-success' | 'alert-warning' | 'alert-danger';
  message: string;
}

export function getProviderTestAlert(result: TestProviderConnectionResult): ProviderTestAlert {
  if (!result.success) {
    return {
      className: 'alert-danger',
      message: `✗ ${result.code ? `[${result.code}] ` : ''}${result.message}`,
    };
  }

  if (result.compatibility?.probe_status !== 'CONFIRMED') {
    return {
      className: 'alert-warning',
      message: `✓ 连接成功，但工具调用能力尚未确认，延迟 ${result.latency_ms}ms`,
    };
  }

  return {
    className: 'alert-success',
    message: `✓ 连接及工具调用已确认，延迟 ${result.latency_ms}ms`,
  };
}
