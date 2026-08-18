import forge from 'node-forge'

// 密码加密传输工具:登录/改密/配置中的密码字段先经 RSA-OAEP-SHA256 加密再上送,
// 避免明文出现在网络/请求体中。公钥由后端 /api/v1/crypto/publicKey 下发(会话级)。
// 使用纯 JS 的 node-forge,不依赖 WebCrypto(后者在局域网 http 非安全上下文不可用)。

let publicKeyPromise = null

// getPublicKey 拉取并缓存会话 RSA 公钥(SPKI PEM)。
export function getPublicKey() {
  if (!publicKeyPromise) {
    publicKeyPromise = fetch('/api/v1/crypto/publicKey', { headers: { Accept: 'application/json' } })
      .then((r) => {
        if (!r.ok) throw new Error(`获取加密公钥失败 (HTTP ${r.status})`)
        return r.json()
      })
      .then((d) => {
        if (!d.publicKey) throw new Error('公钥响应缺少 publicKey')
        return d.publicKey
      })
      .catch((e) => {
        publicKeyPromise = null // 失败后允许下次重试
        throw e
      })
  }
  return publicKeyPromise
}

// encryptField 用 RSA-OAEP-SHA256 加密单个字段,返回 base64 密文;空串原样返回。
// 明文先按 UTF-8 编码为字节(支持中文等非 ASCII 密码),再交给 forge 加密;
// 后端用 rsa.DecryptOAEP(sha256.New(),...) 还原 UTF-8 字节。
export async function encryptField(plain) {
  if (!plain) return ''
  const pem = await getPublicKey()
  const pub = forge.pki.publicKeyFromPem(pem)
  const bytes = new TextEncoder().encode(String(plain))
  const binary = String.fromCharCode(...bytes)
  const encrypted = pub.encrypt(binary, 'RSA-OAEP', {
    md: forge.md.sha256.create(),
    mgf1: { md: forge.md.sha256.create() },
  })
  return forge.util.encode64(encrypted)
}

// isSensitiveField 判断字段是否为敏感凭据(密码/令牌),与后端 isSensitiveField 保持一致。
export function isSensitiveField(name, fieldType) {
  if (fieldType === 'password') return true
  return ['password', 'passwd', 'accesstoken', 'token', 'secret', 'apikey'].includes(
    String(name).toLowerCase(),
  )
}

// encryptSensitive 对 config 对象中所有敏感字段做 RSA 加密(非空才加密),返回新对象。
// schema 用于识别配置中声明为 password 类型的字段;命名约定兜底未声明的密码键。
export async function encryptSensitive(config, schema) {
  const out = { ...config }
  const names = new Set()
  for (const f of schema || []) {
    if (isSensitiveField(f.name, f.type)) names.add(f.name)
  }
  for (const k of Object.keys(config)) {
    if (isSensitiveField(k)) names.add(k)
  }
  for (const name of names) {
    const v = out[name]
    if (v === '' || v === undefined || v === null) continue
    out[name] = await encryptField(String(v))
  }
  return out
}
