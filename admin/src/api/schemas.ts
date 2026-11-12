import { z } from 'zod'

// 管理页面禁止动态代码执行；关闭验证器的运行时编译探测，保持完整结构校验。
z.config({ jitless: true })

const id = z.string().regex(/^[0-9A-HJKMNP-TV-Z]{26}$/)
export const principalSchema = z.object({
  user_id: id,
  session_id: id,
  device_id: id,
  role: z.literal('admin'),
  scope: z.literal('admin'),
})
export const sessionSchema = z.object({
  principal: principalSchema,
  csrf_token: z.string().min(1).max(128),
  access_expires_at: z.iso.datetime({ offset: true }).optional(),
  expires_at: z.iso.datetime({ offset: true }).optional(),
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
      role: z.enum(['admin', 'user']),
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
    }),
  ),
})
export const imagesSchema = z.object({
  images: z.array(z.object({ id, reference: z.string(), is_enabled: z.boolean() })),
})
export const activationSchema = z.object({
  user_id: id,
  activation_token: z.string().min(1).max(256),
  expires_at: z.string(),
})
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
  protocol: z.enum(['openai_compat', 'anthropic']),
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
export const versionSchema = z.object({
  policy_id: id,
  version: z.number(),
  provider_id: id,
  model_id: id,
  credential_id: id.nullable(),
  status: z.enum(['draft', 'published', 'revoked']),
  published_at: z.string().nullable(),
  configuration: z.object({
    model_key: z.string(),
    protocol: z.string(),
    base_url: z.string(),
    context_limit: z.number(),
    output_limit: z.number(),
    supports_tools: z.boolean(),
    supports_vision: z.boolean(),
    prices: z.object({}).passthrough(),
  }),
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
