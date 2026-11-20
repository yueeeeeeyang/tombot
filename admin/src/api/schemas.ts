import { z } from 'zod'

// 管理页面禁止动态代码执行；关闭验证器的运行时编译探测，保持完整结构校验。
z.config({ jitless: true })

const id = z.string().regex(/^[0-9A-HJKMNP-TV-Z]{26}$/)
export const principalSchema = z.object({
  username: z.string().min(1).max(64),
  display_name: z.string().min(1).max(120),
  user_id: id,
  session_id: id,
  device_id: id,
  role: z.literal('admin'),
  scope: z.literal('admin'),
})
export const sessionSchema = z.object({
  principal: principalSchema,
  csrf_token: z.string().min(1).max(128),
  access_expires_at: z.iso.datetime({ offset: true }),
  expires_at: z.iso.datetime({ offset: true }),
})
export type Principal = z.infer<typeof principalSchema>
export const operationSchema = z.object({
  id,
  user_id: id,
  computer_id: id,
  status: z.enum([
    'pending',
    'running',
    'reconciling',
    'compensating',
    'succeeded',
    'failed',
    'quarantined',
  ]),
  stage: z.string(),
  attempt: z.number().int().positive(),
  error_code: z.string().optional(),
  created_at: z.string(),
  updated_at: z.string(),
})
export type Operation = z.infer<typeof operationSchema>
export const operationResult = z.object({ operation: operationSchema })
export const usersSchema = z.object({
  users: z.array(
    z.object({
      id,
      username: z.string(),
      display_name: z.string(),
      status: z.string(),
      computer_status: z.string().nullable(),
      creation_operation_id: id.nullable(),
      created_at: z.string(),
    }),
  ),
})
export type User = z.infer<typeof usersSchema>['users'][number]
export const nodesSchema = z.object({
  nodes: z.array(
    z.object({
      id,
      name: z.string(),
      status: z.string(),
      capacity: z.number().int(),
      reserved: z.number().int(),
      verification_status: z.string().optional(),
      image: z.object({ id, reference: z.string(), is_enabled: z.boolean() }).nullable(),
    }),
  ),
})
const nodeCapabilitiesSchema = z.object({
  terminal: z.boolean(),
  browser: z.boolean().optional(),
  files: z.boolean(),
  process_reaping: z.boolean(),
  disk_quota: z.boolean(),
  network_isolation: z.boolean(),
})
export const nodeDetailSchema = z.object({
  id,
  name: z.string(),
  status: z.string(),
  capacity: z.number().int(),
  reserved: z.number().int(),
  generation: z.number().int(),
  last_seen_at: z.string().optional(),
  capability_expires_at: z.string().optional(),
  configuration_version: z.number().int().positive(),
  is_draining: z.boolean(),
  created_at: z.string(),
  url: z.string().nullable(),
  certificate_fingerprint: z.string().nullable(),
  runtime_id: z.string().nullable(),
  control_expires_at: z.string().nullable(),
  control_sequence: z.number().int(),
  image: z.object({ id, reference: z.string(), is_enabled: z.boolean() }).nullable(),
  upgrade_step: z.string().optional(),
  diagnosis: z.object({ code: z.string(), message: z.string() }),
  can_retry_verification: z.boolean(),
  verification: z
    .object({
      state: z.string(),
      is_valid: z.boolean(),
      attempts: z.number().int(),
      fencing_token: z.number().int(),
      error_code: z.string().nullable(),
      node_error_code: z.string().nullable(),
      updated_at: z.string(),
      next_attempt_at: z.string(),
      verified_at: z.string().nullable(),
      lease_expires_at: z.string().nullable(),
      capabilities: nodeCapabilitiesSchema.nullable(),
      resources: z.object({
        cpu_millis: z.number(),
        memory_bytes: z.number(),
        disk_bytes: z.number(),
        process_limit: z.number(),
      }),
    })
    .nullable(),
})
export type NodeDetail = z.infer<typeof nodeDetailSchema>
export const activationSchema = z.object({
  user_id: id,
  activation_token: z.string().min(1).max(256),
  expires_at: z.string(),
})
export const administratorSchema = z.object({
  id,
  username: z.string(),
  display_name: z.string(),
  status: z.string(),
  created_at: z.string(),
})
export type Administrator = z.infer<typeof administratorSchema>
export const administratorsSchema = z.object({
  administrators: z.array(administratorSchema),
})
// 创建管理员时一次性返回动态验证器密钥；服务端不保存明文，也不提供再次读取。
export const administratorSetupSchema = z.object({
  administrator_id: id,
  username: z.string(),
  display_name: z.string(),
  secret: z.string().regex(/^[A-Z2-7]{32}$/),
  period: z.literal(30),
  digits: z.literal(6),
  algorithm: z.literal('SHA1'),
  created_at: z.string(),
})
export type AdministratorSetup = z.infer<typeof administratorSetupSchema>
export const adminSetupStatusSchema = z.object({ required: z.boolean() })
export const modelMetadataSchema = z.object({
  input_modalities: z.array(z.string()).optional(),
  output_modalities: z.array(z.string()).optional(),
  effort: z
    .object({ supported_levels: z.array(z.string()), default_level: z.string().optional() })
    .optional(),
})
export const remoteModelSchema = z.object({
  model_key: z.string(),
  display_name: z.string(),
  context_limit: z.number().int().positive().optional(),
  output_limit: z.number().int().positive().optional(),
  supports_tools: z.boolean().optional(),
  supports_vision: z.boolean().optional(),
  metadata: modelMetadataSchema,
})
export type RemoteModel = z.infer<typeof remoteModelSchema>
export type ModelMetadata = z.infer<typeof modelMetadataSchema>
export const remoteModelsSchema = z.object({ models: z.array(remoteModelSchema) })
export const enrollmentSchema = z.object({
  secret: z.string().regex(/^[A-Z2-7]{32}$/),
  period: z.literal(30),
  digits: z.literal(6),
  algorithm: z.literal('SHA1'),
})
export type Enrollment = z.infer<typeof enrollmentSchema>
export const providerSchema = z.object({
  id,
  name: z.string(),
  protocol: z.enum(['openai_compat', 'anthropic', 'responses']),
  base_url: z.string(),
  is_enabled: z.boolean(),
  revision: z.number().int().positive(),
  created_at: z.string(),
})
export type Provider = z.infer<typeof providerSchema>
export const modelSchema = z.object({
  id,
  provider_id: id,
  model_key: z.string(),
  display_name: z.string(),
  context_limit: z.number(),
  output_limit: z.number(),
  supports_tools: z.boolean(),
  supports_vision: z.boolean(),
  metadata: modelMetadataSchema,
  reasoning_effort: z.string().optional(),
  is_enabled: z.boolean(),
  revision: z.number(),
})
export type Model = z.infer<typeof modelSchema>
export const credentialSchema = z.object({
  id,
  provider_id: id,
  version: z.number(),
  is_revoked: z.boolean(),
  is_destroyed: z.boolean(),
  created_at: z.string(),
})
export const policySchema = z.object({
  id,
  name: z.string(),
  is_default: z.boolean(),
  is_enabled: z.boolean(),
  revision: z.number(),
  current_version: z.number().nullable(),
})
export type Policy = z.infer<typeof policySchema>
export const candidateSchema = z.object({
  position: z.number().int(),
  provider_id: id,
  model_id: id,
  credential_id: id.nullable(),
  priority: z.number().int(),
  weight: z.number().int(),
  configuration: z.object({
    model_key: z.string(),
    protocol: z.string(),
    base_url: z.string(),
    context_limit: z.number(),
    output_limit: z.number(),
    supports_tools: z.boolean(),
    supports_vision: z.boolean(),
    reasoning_effort: z.string().optional(),
    prices: z.object({}).passthrough(),
  }),
})
export const versionSchema = z.object({
  policy_id: id,
  version: z.number(),
  routing_mode: z.enum(['priority', 'weighted', 'round_robin']),
  candidates: z.array(candidateSchema).min(1).max(16),
  is_enabled: z.boolean(),
  verified_at: z.string().nullable(),
})
export type Version = z.infer<typeof versionSchema>
export const providersSchema = z.object({
  providers: z
    .array(providerSchema)
    .nullable()
    .transform((values) => values ?? []),
})
export const modelsSchema = z.object({
  models: z
    .array(modelSchema)
    .nullable()
    .transform((values) => values ?? []),
})
export const credentialsSchema = z.object({
  credentials: z
    .array(credentialSchema)
    .nullable()
    .transform((values) => values ?? []),
})
export const policiesSchema = z.object({
  policies: z
    .array(policySchema)
    .nullable()
    .transform((values) => values ?? []),
})
export const versionsSchema = z.object({
  versions: z
    .array(versionSchema)
    .nullable()
    .transform((values) => values ?? []),
})
