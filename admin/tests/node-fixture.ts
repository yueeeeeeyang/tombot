import type { NodeDetail } from '../src/api/schemas'
import { id } from './fixtures'

export function nodeDetail(): NodeDetail {
  const now = new Date().toISOString()
  return {
    id: id(4),
    name: '测试电脑节点',
    status: 'bootstrapping',
    capacity: 4,
    reserved: 1,
    generation: 3,
    configuration_version: 2,
    is_draining: false,
    created_at: now,
    last_seen_at: now,
    url: 'https://node.example.test:9444',
    certificate_fingerprint: 'a'.repeat(64),
    runtime_id: id(5),
    control_expires_at: now,
    control_sequence: 120,
    image: { id: id(6), reference: 'computer@sha256:' + 'b'.repeat(64), is_enabled: true },
    diagnosis: {
      code: 'quarantined',
      message: '验收资源状态连续核对失败，已隔离并停止自动重试。修复节点故障后可重新核对原资源。',
    },
    can_retry_verification: true,
    verification: {
      state: 'quarantined',
      is_valid: true,
      attempts: 3,
      fencing_token: 7,
      error_code: 'inspection_unconfirmed',
      node_error_code: 'container_identity_mismatch',
      updated_at: now,
      next_attempt_at: now,
      verified_at: null,
      lease_expires_at: null,
      capabilities: null,
      resources: {
        cpu_millis: 1000,
        memory_bytes: 2147483648,
        disk_bytes: 1073741824,
        process_limit: 512,
      },
    },
  }
}
