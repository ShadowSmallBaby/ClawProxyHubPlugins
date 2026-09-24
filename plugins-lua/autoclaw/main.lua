-- main.lua — AutoClaw 插件（Lua 版）
--
-- 来源：autoclaw-openai 适配器（E:\workspace\projects\web\autoclaw-openai，JS 版，保留为参考）。
-- 把 AutoClaw 桌面端的上游接入核心。上游与客户端头都是固定常量（AutoClaw 客户端内置）。
--
-- 凭据自持：插件自己用 refresh_token 换 access_token（无需桌面端常驻）。
--   授权方式（auth_file）：粘贴 access_token + refresh_token + deviceId 的 JSON。
--
-- 模型目录内置四个官方模型（客户端可见 id = 显示名）；账号编辑页可增删改——
-- 插件不认识的模型名按「名称即上游 id」原样透传。
--
-- 凭据 blob 形态：
--   { "auth": { "accessToken", "refreshToken", "deviceId", "expiresAt" } }

local plugin = {}

local UPSTREAM_WALLET_HOST = "https://autoglm-acceleration-api.zhipuai.cn"
-- 上游对话端点（AutoClaw 客户端固定值；照 JS 版/真实 openclaw.json 提取）
local UPSTREAM_CHAT_BASE = "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw"
-- token 刷新接口（公开可调；请求体 {source_id, device_id, refresh_token}）。
local UPSTREAM_REFRESH_URL = "https://autoglm-acceleration-api.zhipuai.cn/userapi/v1/refresh"
local REFRESH_SOURCE_ID = "autoclaw"
-- AutoClaw 钱包接口签名常量（照 JS 版硬编码）。
local WALLET_APP_ID = "100003"
local WALLET_APP_KEY = "fG4eFqjNqNjr5d1k"
-- 刷新接口签名常量（与钱包同源的 app 级共享密钥，客户端硬编码，非用户凭据）。
local REFRESH_APP_ID = "100003"
local REFRESH_APP_KEY = "38d2391985e2369a5fb8227d8e6cd5e5"
-- access_token 有效期（上游签发 24h；未知时按此兜底计算 expires_at）。
local ACCESS_TOKEN_TTL_MS = 24 * 60 * 60 * 1000
-- 上游代理只接受携带 OpenClaw 插件注入系统块的请求（缺失 → 500 invalid request body）。
local UPSTREAM_SYSTEM_MARKER = "OpenClaw plugin-injected system context."
-- AutoClaw 客户端内置 key（非用户凭据，所有安装一致）。
local CLIENT_API_KEY = "autoclaw-internal-proxy"
-- AutoClaw 客户端固定请求头（所有安装一致）。
local CLIENT_HEADERS = {
  ["X-Tm"] = "win",
  ["X-Version"] = "1.17.9",
  ["X-Product"] = "autoclaw",
  ["X-Channel"] = "official",
  ["X-Lang"] = "zh-CN",
  ["X-Client-Type"] = "pc",
}
-- 内置官方模型目录：id = 上游内部 id，name = 客户端可见显示名。
local DEFAULT_MODELS = {
  { id = "zai_auto", name = "Auto" },
  { id = "zai_auto-fast", name = "Auto-Fast" },
  { id = "zaicoding_glm-5.3", name = "GLM-5.3" },
  { id = "zai_glm-5.3-flash", name = "GLM-5.3-Flash" },
}

-- ============================================================================
-- 基础工具
-- ============================================================================

local function str(v)
  if type(v) == "string" then return v end
  return ""
end

local function num(v)
  if type(v) == "number" then return v end
  return 0
end

local function tbl(v)
  if type(v) == "table" then return v end
  return {}
end

local function or_default(s, def)
  if s == nil or s == "" then return def end
  return s
end

local function fail(msg)
  error(msg, 0)
end

local function trim(s)
  s = string.gsub(str(s), "^%s+", "")
  return (string.gsub(s, "%s+$", ""))
end

-- join_url 折叠重复斜杠（Go/JS: joinUrl）。
local function join_url(base, path)
  return (string.gsub(base or "", "/+$", "")) .. path
end

-- trim_float 浮点转最短十进制字符串。
local function trim_float(x)
  x = num(x)
  if x == math.floor(x) and math.abs(x) < 1e15 then
    return string.format("%d", x)
  end
  local s = string.format("%.14g", x)
  if string.find(s, "[eE]") then
    s = string.format("%.10f", x)
  end
  s = string.gsub(s, "0+$", "")
  s = string.gsub(s, "%.$", "")
  return s
end

-- stream_fail_code 从宿主 stream 错误里还原上游状态码：401 保留，其余 502。
local function stream_fail_code(msg)
  local st = string.match(tostring(msg), "^HTTP (%d+):")
  if st == "401" then return 401 end
  return 502
end

-- is_busy_text busy/限流判定（JS: busy.js 同源关键词，810001 为上游繁忙业务码）。
local function is_busy_text(text)
  text = text or ""
  if string.find(text, "810001", 1, true) then return true end
  local lower = string.lower(text)
  for _, w in ipairs({
    "系统繁忙", "当前使用人数较多", "当前使用人数", "人数较多", "过于繁忙",
    "繁忙", "排队", "拥挤", "稍后", "稍候", "升级模型",
    "too many requests", "too busy", "rate limit", "rate.limit", "overloaded", "load shed",
  }) do
    if string.find(lower, string.lower(w), 1, true) then return true end
  end
  return false
end

-- retry_status 宿主错误文本里的上游状态码（nil = 无状态，如网络失败）。
local function retry_status(msg)
  return string.match(tostring(msg), "^HTTP (%d+)")
end

-- is_retryable_failure 403/429/502/503/504 或 busy 文案 → 可重试（JS: isBusyResponse）。
local function is_retryable_failure(msg)
  local st = retry_status(msg)
  if st == "403" or st == "429" or st == "502" or st == "503" or st == "504" then
    return true
  end
  return is_busy_text(msg)
end

local function truncate(s, n)
  s = s or ""
  if #s <= n then return s end
  return string.sub(s, 1, n)
end

-- http_req 发一次请求，返回 body 字符串 + status；失败返回 nil, err 文本。
local function http_req(method, url, hdrs, body)
  local arg = { method = method, url = url, headers = hdrs or {}, timeout = 60000 }
  if body ~= nil then arg.body = body end
  local ok, resp = pcall(cph.http.request, arg)
  if not ok then
    return nil, tostring(resp)
  end
  return str(resp.body), num(resp.status)
end

-- ============================================================================
-- 凭据（自持 token 对）
-- ============================================================================
--
-- 凭据形态（auth_file 粘贴内容，对齐桌面端 auth.json 的字段名）：
--   { "token": "<access_token>", "refreshToken": "<refresh_token>",
--     "deviceId": "<64位hex>" }
-- deviceId 缺失时从 access_token 的 JWT payload 兜底提取（两者同源）。

-- bare_token 取裸 JWT。上游刷新接口回吐的 token 自带 "Bearer " 前缀，
-- 桌面端解出的凭据同样带前缀；插件内部一律存裸串，拼头时才加自己的 "Bearer "。
local function bare_token(v)
  return (string.gsub(trim(str(v)), "^Bearer%s*", ""))
end

-- jwt_payload 解出 JWT 的 payload table（失败返回 {}）。用于取 device_id / exp。
local function jwt_payload(token)
  local t = bare_token(token)
  local parts = {}
  for seg in string.gmatch(t, "[^.]+") do
    parts[#parts + 1] = seg
  end
  if #parts < 2 then return {} end
  local ok, raw = pcall(cph.hash.base64url_decode, parts[2])
  if not ok or type(raw) ~= "string" then return {} end
  local ok2, payload = pcall(cph.json.decode, raw)
  if not ok2 or type(payload) ~= "table" then return {} end
  return payload
end

-- parse_cred 解析自持凭据 blob（嵌套 auth 形态；兼容 auth.json 的扁平字段名）。
-- token 一律剥掉 "Bearer " 前缀（旧 blob 里可能存了带前缀的值，这里自愈）。
local function parse_cred(blob)
  if blob == nil or blob == "" then
    return nil, "invalid credential: unexpected end of JSON input"
  end
  local ok, c = pcall(cph.json.decode, blob)
  if not ok or type(c) ~= "table" then
    return nil, "invalid credential: malformed JSON"
  end
  local auth = c.auth
  if type(auth) ~= "table" then auth = {} end
  local cred = {
    access_token = bare_token(or_default(str(auth.accessToken), or_default(str(c.token), str(c.accessToken)))),
    refresh_token = bare_token(or_default(str(auth.refreshToken), str(c.refreshToken))),
    device_id = or_default(str(auth.deviceId), str(c.deviceId)),
    expires_at = num(or_default(auth.expiresAt, c.expiresAt)),
  }
  if cred.access_token == "" and cred.refresh_token == "" then
    return nil, "credential missing token/refreshToken（凭据需含 access_token 与 refresh_token）"
  end
  return cred
end

-- cred_to_blob 序列化自持凭据（嵌套 auth 形态，插件内部唯一凭据载体）。
local function cred_to_blob(cred)
  return cph.json.encode({
    auth = {
      accessToken = cred.access_token,
      refreshToken = cred.refresh_token,
      expiresAt = cred.expires_at,
      deviceId = cred.device_id,
    },
  })
end

-- cred_from 解析使用侧凭据（CredentialBlob 包装）。
-- 仅支持自持形态；旧版 local_config / jwt 形态提示重新授权。
local function cred_from(cred_blob)
  if type(cred_blob) ~= "table" then
    return nil, "invalid credential: unexpected end of JSON input"
  end
  local raw = str(cred_blob.blob)
  if raw == "" then
    return nil, "invalid credential: unexpected end of JSON input"
  end
  local ok, c = pcall(cph.json.decode, raw)
  if not ok or type(c) ~= "table" then
    return nil, "invalid credential: malformed JSON"
  end
  -- 旧版形态：local_config（文件）/ jwt（粘贴 JWT）已移除，提示重建账号
  if str(c.source) == "runtime_file" or str(c.source) == "jwt" or trim(str(c.jwt)) ~= "" then
    return nil, "invalid credential: 旧版凭据形态已不再支持（插件已改为自持 refresh_token），请删除账号后重新授权"
  end
  local cred, cerr = parse_cred(raw)
  if cred == nil then
    return nil, cerr
  end
  cred.source = "self_managed"
  -- deviceId 缺失时从 AT 的 JWT payload 兜底提取（auth.json 里 deviceId 与 AT 同源）
  if trim(cred.device_id) == "" then
    local payload = jwt_payload(cred.access_token)
    cred.device_id = or_default(str(payload.device_id), str(payload.deviceId))
  end
  return cred
end

-- jwt_for 取 access_token。自持凭据直接用 blob 内的 AT（过期由 refresh 换新）。
-- 返回 jwt, err。
local function jwt_for(cred)
  if cred.source == "self_managed" then
    if trim(cred.access_token) ~= "" then
      return cred.access_token
    end
    return nil, "凭据里没有 access_token：请点刷新换取新 token，或重新粘贴凭据"
  end
  return nil, "凭据里没有 JWT，请更新账号凭据"
end

-- ============================================================================
-- token 刷新（自持凭据：用 refresh_token 换新 access_token）
-- ============================================================================

-- refresh_headers 刷新接口请求头（app 级签名，与钱包头同源）。
local function refresh_headers(access_token)
  local ts = math.floor(cph.time.now() / 1000)
  local sign = cph.hash.md5(REFRESH_APP_ID .. "&" .. ts .. "&" .. REFRESH_APP_KEY)
  local h = {
    ["Accept"] = "application/json",
    ["Content-Type"] = "application/json",
    ["X-Auth-Appid"] = REFRESH_APP_ID,
    ["X-Auth-TimeStamp"] = tostring(ts),
    ["X-Auth-Sign"] = sign,
    ["X-Trace-Id"] = cph.random.uuid(),
    ["X-Lang"] = "zh-CN",
  }
  for k, v in pairs(CLIENT_HEADERS) do h[k] = v end
  if access_token ~= "" then
    h["authorization"] = "Bearer " .. access_token
  end
  return h
end

-- refresh_access_token 用 refresh_token 换新 access_token，原地更新 cred。
-- 成功返回新 AT；失败返回 nil, 提示（status 为上游 HTTP 状态码，401 = RT 失效）。
local function refresh_access_token(cred)
  if trim(cred.refresh_token) == "" then
    return nil, "凭据里没有 refreshToken，请重新粘贴凭据"
  end
  if trim(cred.device_id) == "" then
    return nil, "凭据里没有 deviceId：请从桌面端 auth.json 复制 deviceId 后重新粘贴"
  end

  local body = cph.json.encode({
    source_id = REFRESH_SOURCE_ID,
    device_id = cred.device_id,
    refresh_token = cred.refresh_token,
  })
  local raw, status = http_req("POST", UPSTREAM_REFRESH_URL, refresh_headers(cred.access_token), body)
  if raw == nil then
    return nil, "刷新请求失败：" .. tostring(status)
  end

  local ok, resp = pcall(cph.json.decode, raw)
  if not ok or type(resp) ~= "table" then
    return nil, "刷新响应无法解析（HTTP " .. tostring(status) .. "）"
  end
  -- 凭据被吊销：让 Go 侧把账号标记为 expired（错误码必须是 401）
  local code = num(resp.code)
  if status == 401 or code == 400000 or code == 410000 then
    return nil, "refresh_token 已失效（" .. or_default(str(resp.msg), "HTTP " .. tostring(status)) .. "）：请重新粘贴凭据"
  end
  if code ~= 0 then
    return nil, "刷新失败（code " .. tostring(code) .. "）："
      .. or_default(str(resp.msg), "HTTP " .. tostring(status))
  end

  local d = tbl(resp.data)
  local at = bare_token(d.access_token)
  if at == "" then
    return nil, "刷新响应缺少 access_token"
  end
  cred.access_token = at
  -- RT 不轮换（实测），但服务端若回吐新值则一并接受（与 workbuddy 同策略）。
  local rt = bare_token(d.refresh_token)
  if rt ~= "" then cred.refresh_token = rt end

  local expires_in = num(d.expires_in)
  if expires_in > 0 then
    cred.expires_at = cph.time.now() + expires_in * 1000
  else
    cred.expires_at = cph.time.now() + ACCESS_TOKEN_TTL_MS
  end
  return at
end

-- ============================================================================
-- 短信验证码登录（文档 §5 已实测闭环）：发码 → 登录 → 自持 RT（约 30 天）
-- ============================================================================

-- agent_login_headers 登录接口签名头（与刷新同源同套路；无 AT 时省略 authorization）。
local function agent_login_headers()
  return refresh_headers("")
end

-- agent_send_code 发送短信验证码。成功判定：code == 0 且 data.result == true。
local function agent_send_code(phone, device_id)
  local body = cph.json.encode({
    phone = phone,
    source_id = REFRESH_SOURCE_ID,
    device_id = device_id,
  })
  local raw, status = http_req("POST", UPSTREAM_WALLET_HOST .. "/userapi/v1/agent-send-code",
    agent_login_headers(), body)
  if raw == nil then
    return "发码请求失败：" .. tostring(status)
  end
  local ok, resp = pcall(cph.json.decode, raw)
  if not ok or type(resp) ~= "table" then
    return "发码响应无法解析（HTTP " .. tostring(status) .. "）"
  end
  if num(resp.code) ~= 0 or tbl(resp.data).result ~= true then
    return or_default(str(resp.msg), "发码失败（code " .. num(resp.code) .. "）")
  end
  return nil
end

-- agent_login 验证码换 token 对。code 转整数（客户端同款行为；num() 只认 number，
-- 字符串验证码必须用 tonumber）。
-- 返回 access_token, refresh_token, user_name, err；失败前三个为 nil。
local function agent_login(phone, code, device_id)
  local body = cph.json.encode({
    phone = phone,
    code = tonumber(code) or 0,
    source_id = REFRESH_SOURCE_ID,
    device_id = device_id,
  })
  local raw, status = http_req("POST", UPSTREAM_WALLET_HOST .. "/userapi/v1/agent-login",
    agent_login_headers(), body)
  if raw == nil then
    return nil, nil, nil, "登录请求失败：" .. tostring(status)
  end
  local ok, resp = pcall(cph.json.decode, raw)
  if not ok or type(resp) ~= "table" then
    return nil, nil, nil, "登录响应无法解析（HTTP " .. tostring(status) .. "）"
  end
  local c = num(resp.code)
  if c ~= 0 then
    -- 400001 格式 / 630006 号码不合法 / 630201 过期 / 630202 错误 → 400（用户输入问题）
    return nil, nil, or_default(str(resp.msg), "登录失败（code " .. c .. "）")
  end
  local d = tbl(resp.data)
  local rt = bare_token(d.refresh_token)
  local at = bare_token(d.access_token)
  if rt == "" or at == "" then
    return nil, nil, nil, "登录响应缺少 token"
  end
  return at, rt, or_default(str(d.user_name), "")
end

-- ============================================================================
-- 模型（内置官方目录；账号编辑页可增删改，未知名称按上游 id 原样透传）
-- ============================================================================

-- resolve_upstream_model 客户端可见名 → 上游内部 id。
-- 优先精确匹配（显示名/内部 id，忽略大小写兜底）；都不中则原样透传。
local function resolve_upstream_model(model)
  if model == nil or model == "" then return model end
  local lower = string.lower(model)
  for _, m in ipairs(DEFAULT_MODELS) do
    if str(m.name) == model or str(m.id) == model then
      return str(m.id)
    end
  end
  for _, m in ipairs(DEFAULT_MODELS) do
    if string.lower(str(m.name)) == lower or string.lower(str(m.id)) == lower then
      return str(m.id)
    end
  end
  return model
end

-- ============================================================================
-- 请求构造（JS: withUpstreamMarker / buildHeaders）
-- ============================================================================

-- with_upstream_marker 上游要求会话携带 OpenClaw 插件注入系统块，缺失则补一行。
local function with_upstream_marker(payload)
  local messages = payload.messages
  if type(messages) ~= "table" then return payload end
  for _, m in ipairs(messages) do
    if type(m) == "table" and m.role == "system" and type(m.content) == "string"
       and string.find(m.content, UPSTREAM_SYSTEM_MARKER, 1, true) then
      return payload
    end
  end
  local out = {}
  for k, v in pairs(payload) do out[k] = v end
  -- 不用 unpack：ZCode 的长会话数千条消息会超 Lua 栈上限（registry overflow）
  local msgs = { { role = "system", content = UPSTREAM_SYSTEM_MARKER } }
  for i, m in ipairs(messages) do
    msgs[i + 1] = m
  end
  out.messages = msgs
  return out
end

-- build_headers 固定客户端头 + 内置 key + 每请求的模型 id 与 JWT。
local function build_headers(jwt, upstream_id)
  local h = {
    ["Content-Type"] = "application/json",
    ["Authorization"] = "Bearer " .. CLIENT_API_KEY,
    ["X-Request-Model"] = upstream_id,
    ["X-Authorization"] = "Bearer " .. jwt,
    ["X-Trace-Id"] = cph.random.uuid(),
  }
  for k, v in pairs(CLIENT_HEADERS) do h[k] = v end
  return h
end

-- ============================================================================
-- Manifest / 登录
-- ============================================================================

function plugin.handshake(req)
  if num(req.protocol_version) ~= 2 then
    return {
      error = {
        code = 1,
        message = string.format("protocol mismatch: core=%d plugin=2", num(req.protocol_version)),
      },
    }
  end
  return {
    manifest = {
      name = "autoclaw",
      version = "0.5.0",
      author = "cph",
      label = { zh = "AutoClaw", en = "AutoClaw" },
      protocol_version = 2,
      capabilities = { "chat", "models", "login", "refresh" },
      endpoints = { "chat_completions", "messages" },
      auth_methods = {
        {
          id = "auth_file",
          label = { zh = "凭据内容", en = "Credential Content" },
          capabilities = { "refreshable" },
          fields = {
            {
              name = "content",
              label = {
                zh = "凭据 JSON（token / refreshToken / deviceId）",
                en = "Credential JSON (token / refreshToken / deviceId)",
              },
              type = "textarea",
              required = true,
              placeholder = '{"token": "access_token", "refreshToken": "refresh_token", "deviceId": "64位hex"}',
            },
          },
        },
        {
          -- 验证码登录（文档 §5 已实测闭环）：发码 → 输码 → 换取自持 RT（约 30 天）。
          -- agent 系 token 实测可直接调模型与钱包；与桌面端凭证链并存互不干扰。
          id = "phone_otp",
          label = { zh = "手机验证码登录", en = "Phone OTP Login" },
          capabilities = { "refreshable" },
          fields = {
            { name = "phone", label = { zh = "手机号", en = "Phone Number" }, type = "phone", required = true },
          },
        },
      },
    },
  }
end

-- ============================================================================
-- 登录
-- ============================================================================

function plugin.login(req)
  local method = str(req.method_id)

  if method == "auth_file" then
    -- 粘贴 access_token + refresh_token + deviceId 的 JSON。
    -- access_token 可留空，由首次换票补齐。
    local content = trim(str(tbl(req.form).content))
    if content == "" then
      return { error = { code = 400, message = "请粘贴凭据内容（含 token 与 refreshToken）" } }
    end
    local cred, cerr = parse_cred(content)
    if cred == nil then
      return { error = { code = 400, message = cerr } }
    end
    if trim(cred.refresh_token) == "" then
      return { error = { code = 400, message = "凭据缺少 refreshToken，无法自动刷新" } }
    end
    if trim(cred.device_id) == "" then
      return { error = { code = 400, message = "凭据缺少 deviceId：refresh 接口必填，请从 auth.json 复制" } }
    end
    -- access_token 缺省时先换一次票，确保账号立即可用
    if trim(cred.access_token) == "" then
      local at, rerr = refresh_access_token(cred)
      if at == nil then
        return { error = { code = 401, message = rerr } }
      end
    end
    return {
      blob = cred_to_blob(cred),
      profile = {
        display_name = "AutoClaw",
        healthy = true,
        quota = {},
      },
    }
  end

  if method == "phone_otp" then
    -- 两步流（state: "otp:<phone>:<device_id>"）：
    -- 第一步发码并回传下一步表单；第二步用验证码换 token 对，凭据归入自持 RT。
    local state = str(req.state)
    if state == "" then
      local phone = trim(str(tbl(req.form).phone))
      if phone == "" or not string.match(phone, "^1%d+$") then
        return { error = { code = 400, message = "请填写 11 位手机号" } }
      end
      local device_id = cph.random.hex(32) -- 64 位 hex，与桌面端 device_id 同形态
      local err = agent_send_code(phone, device_id)
      if err ~= nil then
        return { error = { code = 400, message = err } }
      end
      return {
        next = {
          action = "input_form",
          prompt = {
            zh = "验证码已发送，请输入收到的 6 位短信验证码",
            en = "OTP sent; enter the 6-digit code you received via SMS",
          },
          fields = {
            { name = "code", label = { zh = "验证码", en = "SMS Code" }, type = "text", required = true },
          },
          state = "otp:" .. phone .. ":" .. device_id,
        },
      }
    end

    local phone, device_id = string.match(state, "^otp:([^:]+):(.+)$")
    if phone == nil or device_id == nil then
      return { error = { code = 401, message = "登录会话已失效，请重新发起" } }
    end
    local code = trim(str(tbl(req.form).code))
    if not string.match(code, "^%d%d%d%d%d%d$") then
      return { error = { code = 400, message = "验证码应为 6 位数字" } }
    end
    local at, rt, uname, lerr = agent_login(phone, code, device_id)
    if lerr ~= nil then
      -- 630201/630202 等用户输入问题 → 400；其余 502
      local code400 = string.match(lerr, "630201") or string.match(lerr, "630202")
        or string.match(lerr, "400001") or string.match(lerr, "630006")
      return { error = { code = code400 and 400 or 502, message = lerr } }
    end
    local cred = {
      access_token = at,
      refresh_token = rt,
      device_id = device_id,
      expires_at = 0, -- 首次使用时若过期会由网关 401 恢复链路刷新
    }
    return {
      blob = cred_to_blob(cred),
      profile = {
        display_name = or_default(uname, "AutoClaw"),
        healthy = true,
        quota = {},
      },
    }
  end

  fail("unknown auth method: " .. method)
end

-- ============================================================================
-- 模型目录（内置官方目录；客户端可见 id = 显示名）
-- ============================================================================

function plugin.models(cred_blob)
  local models = {}
  for _, m in ipairs(DEFAULT_MODELS) do
    models[#models + 1] = {
      id = or_default(str(m.name), str(m.id)),
      context_window = 1048576,
      supports_tools = true,
      supports_stream = true,
    }
  end
  return { models = models }
end

-- ============================================================================
-- Chat（JS: handleChatCompletions 流式路径）
-- ============================================================================

function plugin.chat(req, stream)
  local cred, err = cred_from(req.credential)
  if not cred then
    stream.failed({ code = 401, message = or_default(err, "invalid credential") })
    return
  end
  local jwt, jerr = jwt_for(cred)
  if jwt == nil then
    stream.failed({ code = 401, message = jerr })
    return
  end

  local upstream_id = resolve_upstream_model(str(req.model))
  local payload = cph.openai.chat_body(req)
  payload.model = upstream_id
  payload = with_upstream_marker(payload)
  if payload.stream_options ~= nil then
    payload.stream_options = nil
  end

  -- 消息开始事件只发一次，带客户端使用的别名（等价 JS 的 chunk.model 重写）。
  -- 之后重试不再重复发：失败的尝试不会有任何事件到达客户端（429 等在泵出前被拒）。
  stream.message_start({ model = req.model })

  -- 重试循环（JS: RETRY_MAX=6 / RETRY_BASE_MS=1500 线性退避）：
  -- 403/429/502/503/504 或 busy 文案 → 退避后整体重试；401/其余错误立即失败。
  -- 401 交给网关 recoverCredential 兜底（刷新同账号后重试），插件不自行换票。
  local max_attempts = 6
  local attempt = 0
  local last_err = ""
  while attempt < max_attempts do
    attempt = attempt + 1
    if attempt > 1 then
      cph.time.sleep(1500 * (attempt - 1))
    end
    local ok, serr = cph.http.stream({
      method = "POST",
      url = join_url(UPSTREAM_CHAT_BASE, "/chat/completions"),
      headers = build_headers(jwt, upstream_id),
      body = payload,
      credential = req.credential,
      stream = stream,
      format = "openai",
    })
    if ok then
      return -- 已正常完成（finish 事件由宿主解析器发出）
    end
    last_err = tostring(serr)
    if retry_status(last_err) == "401" then
      stream.failed({ code = 401, message = last_err }) -- token 过期：不重试
      return
    end
    if not is_retryable_failure(last_err) or attempt >= max_attempts then
      break
    end
  end

  stream.failed({
    code = stream_fail_code(last_err),
    message = "upstream busy after " .. max_attempts .. " attempts: " .. truncate(last_err, 300),
  })
end

-- ============================================================================
-- 积分（JS: handleAutoclawBalance —— 钱包总额，签名头 + Bearer JWT）
-- ============================================================================

-- wallet_headers 钱包请求头：md5 签名（appid&timestamp&appkey）+ Bearer JWT。
-- X-Version 与对话头共用 CLIENT_HEADERS 的同一个值（客户端真实版本 1.17.9，勿分开写）。
local function wallet_headers(jwt)
  local ts = math.floor(cph.time.now() / 1000)
  local sign = cph.hash.md5(WALLET_APP_ID .. "&" .. ts .. "&" .. WALLET_APP_KEY)
  local h = {
    ["Accept"] = "*/*",
    ["Content-Type"] = "application/json",
    ["X-Version"] = CLIENT_HEADERS["X-Version"],
    ["X-Tm"] = "autoclaw",
    ["X-Product"] = "autoclaw",
    ["X-Auth-Appid"] = WALLET_APP_ID,
    ["X-Auth-TimeStamp"] = tostring(ts),
    ["X-Auth-Sign"] = sign,
    ["X-Trace-Id"] = cph.random.uuid(),
    ["X-Lang"] = "zh-CN",
    ["X-Channel"] = "official",
    ["authorization"] = "Bearer " .. jwt,
  }
  return h
end

-- plugin.refresh 刷新凭据：用 refresh_token 换新 access_token，回吐新 blob 落库。
-- 换票结果是成败的唯一依据；钱包积分查询失败只影响快照，不影响刷新结论。
function plugin.refresh(cred_blob)
  local cred, err = cred_from(cred_blob)
  if not cred then
    return { error = { code = 400, message = err } }
  end

  local _, rerr = refresh_access_token(cred)
  if rerr ~= nil then
    -- RT 被吊销 / 缺字段 → 401（Go 侧标记 expired）；上游故障 → 502（保留账号原状）
    local code = 502
    if string.find(rerr, "refresh_token 已失效", 1, true)
      or string.find(rerr, "没有 deviceId", 1, true)
      or string.find(rerr, "没有 refreshToken", 1, true) then
      code = 401
    end
    return { error = { code = code, message = rerr } }
  end

  local profile = {
    display_name = "AutoClaw",
    healthy = true,
    quota = {},
  }
  -- 钱包余额是附加信息：查不到就返回基本档案，绝不因此判定刷新失败
  local jwt = jwt_for(cred)
  if jwt ~= nil then
    local url = UPSTREAM_WALLET_HOST .. "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
    local raw, status = http_req("GET", url, wallet_headers(jwt))
    if raw ~= nil and status ~= 401 then
      local ok, body = pcall(cph.json.decode, raw)
      if ok and type(body) == "table" and num(body.code) == 0
        and type(tbl(body.data).total_balance) == "number" then
        local total = trim_float(num(body.data.total_balance))
        profile.quota.credits = total
        profile.credits_json = cph.json.encode({ remaining = total, total = total })
      end
    end
  end
  return { blob = cred_to_blob(cred), profile = profile }
end

function plugin.profile(cred_blob)
  local cred, err = cred_from(cred_blob)
  if not cred then
    fail(err)
  end
  local profile = {
    display_name = "AutoClaw",
    healthy = true,
    quota = {},
  }
  local jwt = jwt_for(cred)
  if jwt == nil then
    return profile -- 文件不可读等：静默降级为基本档案
  end
  local url = UPSTREAM_WALLET_HOST .. "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
  local raw, status = http_req("GET", url, wallet_headers(jwt))
  if raw == nil then
    return profile -- 静默降级：钱包失败不影响基本档案
  end
  local ok, body = pcall(cph.json.decode, raw)
  if ok and type(body) == "table" and num(body.code) == 0 and type(tbl(body.data).total_balance) == "number" then
    local total = trim_float(num(body.data.total_balance))
    profile.quota.credits = total
    profile.credits_json = cph.json.encode({ remaining = total, total = total })
  end
  return profile
end

return plugin
