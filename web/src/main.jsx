import React, { useEffect, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { ArrowUp, Check, ChevronDown, CircleAlert, Command, LogOut, MessageSquare, Plus, Settings2, Square, X } from 'lucide-react'
import { Button } from './components/ui/button'
import './style.css'

const invitation = new URLSearchParams(location.hash.slice(1))
const initialCode = invitation.get('code') || ''
const pinned = Boolean(invitation.get('pin'))
if (location.hash) history.replaceState(null, '', location.pathname + location.search)
const uid = () => crypto.randomUUID()
const credentialKey = 'serverside.web.credentials.v1'

async function jsonRequest(path, options = {}) {
  const response = await fetch(path, { ...options, cache: 'no-store' })
  const body = await response.json().catch(() => ({}))
  if (!response.ok || body.ok === false) {
    const err = new Error(body.error?.message || `Request failed (${response.status})`)
    err.status = response.status
    err.code = body.error?.code
    throw err
  }
  return body
}
function App() {
  const [code, setCode] = useState(initialCode), [connected, setConnected] = useState(false)
  const [chats, setChats] = useState([]), [active, setActive] = useState(''), [conversation, setConversation] = useState(null)
  const [draft, setDraft] = useState(''), [error, setError] = useState(''), [busy, setBusy] = useState(false), [stream, setStream] = useState('')
  const [settingsOpen, setSettingsOpen] = useState(false), [source, setSource] = useState('stored')
  const [provider, setProvider] = useState(''), [model, setModel] = useState(''), [providerType, setProviderType] = useState('openai')
  const [baseURL, setBaseURL] = useState(''), [apiKey, setApiKey] = useState(''), [keyID, setKeyID] = useState('')
  const [pending, setPending] = useState(false)
  const auth = useRef(null), activeRef = useRef(''), running = useRef(''), abort = useRef(null), bottom = useRef(null)

  async function post(path, body, token, signal) {
    return jsonRequest(path, { method:'POST', headers:{'Content-Type':'application/json', ...(token ? {Authorization:`Bearer ${token}`} : {})}, body:JSON.stringify(body), signal })
  }
  async function rpc(op, data = {}, sessionId = '', signal) {
    const call = async () => post('/v1/rpc', {id:uid(), op, ...(sessionId ? {session_id:sessionId} : {}), data}, auth.current.token, signal)
    try { return (await call()).data }
    catch (e) {
      if (e.status !== 401 || !auth.current) throw e
      const renewed = await post('/v1/auth', {client_id:auth.current.client_id, secret:auth.current.secret}, null, signal)
      auth.current.token = renewed.token
      return (await call()).data
    }
  }
  useEffect(() => {
    let cancelled = false
    try {
      const saved = JSON.parse(localStorage.getItem(credentialKey) || 'null')
      if (!saved?.client_id || !saved?.secret) return
      post('/v1/auth', saved).then(login => {
        if (cancelled) return
        auth.current = {...saved, token:login.token}
        setConnected(true)
      }).catch(() => {
        if (!cancelled) { localStorage.removeItem(credentialKey); setError('Phiên ghép cặp đã hết hiệu lực. Hãy ghép cặp lại.') }
      })
    } catch { localStorage.removeItem(credentialKey) }
    return () => { cancelled = true }
  }, [])
  async function connect(e) {
    e.preventDefault(); setError(''); setPending(true)
    try {
      const paired = await post('/v1/pair', {code:code.trim()})
      setCode('')
      const login = await post('/v1/auth', {client_id:paired.client_id, secret:paired.secret})
      auth.current = {client_id:paired.client_id, secret:paired.secret, token:login.token}
      localStorage.setItem(credentialKey, JSON.stringify({client_id:paired.client_id, secret:paired.secret}))
      setConnected(true)
    } catch (e) { setError(e.message) } finally { setPending(false) }
  }
  function disconnect() {
    abort.current?.abort(); auth.current = null; localStorage.removeItem(credentialKey); activeRef.current = ''; running.current = ''
    setConnected(false); setActive(''); setConversation(null); setChats([]); setBusy(false); setStream(''); setApiKey(''); setError('')
  }
  async function loadChats() {
    const all = []; let offset = 0
    do { const page = await rpc('session.list', {offset,limit:100}); all.push(...page.items.filter(x=>!x.deleted_at)); offset = page.next_offset } while (offset)
    setChats(all.sort((a,b)=>(b.updated_at||0)-(a.updated_at||0)))
  }
  useEffect(() => { if (connected) loadChats().catch(e=>setError(e.message)) }, [connected])
  useEffect(() => { bottom.current?.scrollIntoView({behavior:'smooth'}) }, [conversation, stream])
  useEffect(() => () => abort.current?.abort(), [])
  function config() {
    if (!provider.trim() && source === 'stored') throw Error('Nhập provider ID trong phần Model trước khi gửi.')
    if (!model.trim()) throw Error('Nhập model ID trong phần Model trước khi gửi.')
    if (source === 'stored') return {source:'stored',provider_id:provider.trim(),model_id:model.trim(),...(keyID.trim() ? {key_id:keyID.trim()} : {})}
    if (!apiKey.trim()) throw Error('Nhập API key trong phần Model trước khi gửi.')
    return {source:'mobile_ephemeral',provider:{type:providerType,modelId:model.trim(),...(baseURL.trim()?{baseUrl:baseURL.trim()}:{}),auth:{method:'bearer_token',value:apiKey}}}
  }
  async function openChat(id, runtime) {
    abort.current?.abort(); running.current = ''; setBusy(false); setStream(''); setError('')
    activeRef.current = id; setActive(id); setConversation(null)
    const result = await rpc('session.open', runtime ? {runtime} : {}, id)
    if (activeRef.current === id) setConversation(result.conversation)
  }
  async function newChat() {
    const id = uid(); try { await openChat(id); await loadChats() } catch(e) { setError(e.message) }
  }
  async function chooseChat(id) { if (busy) return; try { await openChat(id) } catch(e) { setError(e.message) } }
  async function refreshSnapshot(id) {
    const result = await rpc('session.get', {}, id)
    if (activeRef.current === id) { setConversation(result.conversation); setStream('') }
  }
  async function poll(id, requestID, signal) {
    let cursor = 0
    while (!signal.aborted && activeRef.current === id) {
      let page
      try { page = await jsonRequest(`/v1/events?session_id=${encodeURIComponent(id)}&after=${cursor}`, {headers:{Accept:'application/json',Authorization:`Bearer ${auth.current.token}`}, signal}) }
      catch(e) {
        if (e.name === 'AbortError') return
        if (e.status === 401 && auth.current) { const login = await post('/v1/auth', {client_id:auth.current.client_id, secret:auth.current.secret}, null, signal); auth.current.token = login.token; continue }
        throw e
      }
      for (const event of page.events || []) {
        cursor = event.seq
        if (event.request_id !== requestID) continue
        if (event.kind === 'delta') setStream(v => v + (event.data?.text || ''))
        if (event.kind === 'conversation') { setConversation(event.data.conversation); setStream('') }
        if (event.kind === 'completed' || event.kind === 'error') {
          await refreshSnapshot(id); await loadChats()
          if (event.kind === 'error') setError(`Không thể hoàn tất: ${event.data?.code || 'lỗi server'}`)
          return
        }
      }
    }
  }
  async function send(e) {
    e.preventDefault(); const content = draft.trim(); if (!content || busy) return
    setError(''); let runtime
    try { runtime = config() } catch(e) { setError(e.message); setSettingsOpen(true); return }
    let id = active; const controller = new AbortController(); setBusy(true)
    try {
      if (!id) { id = uid(); await openChat(id, runtime) } else await rpc('session.configure', {runtime}, id)
      abort.current = controller; setBusy(true)
      // Never automatically replay a run whose outcome is uncertain.
      const requestID = uid(); running.current = requestID
      await rpc('session.run', {request_id:requestID,content}, id)
      setDraft(''); await poll(id, requestID, controller.signal)
    } catch(e) {
      if (e.name !== 'AbortError') { setError(`${e.message}. Kiểm tra lại hội thoại trước khi gửi lại.`); if (id) await refreshSnapshot(id).catch(()=>{}) }
    } finally { if (abort.current === controller) { running.current=''; setBusy(false); setStream(''); abort.current=null } }
  }
  async function cancel() { if (!active || !running.current) return; try { await rpc('session.cancel',{request_id:running.current},active) } catch(e) { setError(e.message) } }
  const messages = conversation?.messages || []
  return <div className="app">
    <aside className="sidebar"><div className="brand"><div className="brandmark"><Command size={18}/></div><div><strong>serverside</strong><small>YOUR AI WORKSPACE</small></div></div>
      <button className="newchat" disabled={!connected || busy} onClick={newChat}><Plus size={17}/> Cuộc trò chuyện mới</button>
      <div className="side-label">GẦN ĐÂY <span>{chats.length}</span></div><div className="chat-list">{chats.map(chat=><button key={chat.id} onClick={()=>chooseChat(chat.id)} className={active === chat.id ? 'chat-item selected':'chat-item'}><MessageSquare size={15}/><span>{chat.title || 'Cuộc trò chuyện mới'}</span></button>)}{connected && !chats.length && <p className="empty-list">Chưa có cuộc trò chuyện nào.</p>}</div>
      <div className="side-bottom"><button onClick={()=>setSettingsOpen(true)} disabled={!connected}><Settings2 size={17}/><span>Cài đặt model</span><ChevronDown size={14}/></button>{connected && <button onClick={disconnect}><LogOut size={17}/><span>Ngắt kết nối</span></button>}</div></aside>
    <main className="main"><header className="topbar"><div><span className="top-icon"><MessageSquare size={15}/></span> {active ? (chats.find(c=>c.id===active)?.title || 'Cuộc trò chuyện mới') : 'Trò chuyện'}</div><div className="topright"><span className="status"><i className={connected?'online':''}/>{connected?'Đã kết nối':'Chưa kết nối'}</span><button onClick={()=>setSettingsOpen(true)} disabled={!connected} className="model-pill"><span/>{model || 'Chọn model'}<ChevronDown size={14}/></button></div></header>
      {!connected ? <div className="login-wrap"><div className="login-card"><div className="login-icon"><Command size={24}/></div><div className="eyebrow">KẾT NỐI AN TOÀN</div><h1>Chào mừng trở lại.</h1><p>Nhập mã ghép cặp để bắt đầu trò chuyện với server của bạn. Khóa kết nối sẽ được lưu trong trình duyệt để bạn mở lại lịch sử.</p><form onSubmit={connect}><label htmlFor="pair">MÃ GHÉP CẶP</label><input id="pair" value={code} onChange={e=>setCode(e.target.value)} placeholder="Dán mã ghép cặp" autoComplete="off" required/><Button disabled={pending || !code.trim()}>{pending?'Đang kết nối…':'Kết nối'}</Button></form><div className="login-note">Trên máy chạy server, tạo mã có hiệu lực 2 phút:<code>./generative pair --stdio --transport http-local</code>{pinned && <span>Trình duyệt không thể xác minh SPKI pin; hãy tự xác minh chứng chỉ HTTPS trước khi ghép cặp.</span>}</div>{error && <div className="error" role="alert"><CircleAlert size={15}/>{error}</div>}</div></div> : <><div className="conversation"><div className="thread">{!messages.length && <div className="welcome"><div className="welcome-icon"><Command size={25}/></div><span className="eyebrow">MỘT KHÔNG GIAN ĐỂ SUY NGHĨ</span><h1>Hôm nay bạn muốn<br/><em>khám phá điều gì?</em></h1><p>Đặt câu hỏi, phát triển ý tưởng hoặc bắt đầu một cuộc trò chuyện mới.</p></div>}{messages.map((msg,i)=>{const version=msg.versions?.[msg.active_version_index || 0]; let content=version?.content || ''; if (i===messages.length-1 && msg.role==='model' && stream) content += stream; return <article className={'message '+(msg.role==='user'?'from-user':'from-ai')} key={msg.id}><div className="avatar">{msg.role==='user'?'B':<Command size={15}/>}</div><div className="message-body"><div className="message-label">{msg.role==='user'?'BẠN':'ASSISTANT'}{version?.generation_interrupted && busy && <span> ĐANG TRẢ LỜI</span>}</div><div className="message-text">{content || (busy && msg.role==='model'?'Đang suy nghĩ…':'')}</div></div></article>})}<div ref={bottom}/></div></div><div className="composer-area">{error && <div className="error" role="alert"><CircleAlert size={15}/>{error}<button onClick={()=>setError('')} aria-label="Đóng"><X size={14}/></button></div>}<form className="composer" onSubmit={send}><textarea value={draft} onChange={e=>setDraft(e.target.value)} onKeyDown={e=>{if(e.key==='Enter' && !e.shiftKey && !e.nativeEvent.isComposing){e.preventDefault();e.currentTarget.form.requestSubmit()}}} placeholder="Nhắn tin cho assistant..." rows="2"/><div className="composer-foot"><span><span className="mini-dot"/> {model || 'Chọn model để bắt đầu'}</span>{busy ? <button type="button" className="send" onClick={cancel} aria-label="Dừng"><Square size={16} fill="currentColor"/></button> : <button className="send" disabled={!draft.trim()} aria-label="Gửi tin nhắn"><ArrowUp size={18}/></button>}</div></form><p className="disclaimer">Nội dung hội thoại được lưu trên server của bạn. · Enter để gửi, Shift + Enter để xuống dòng</p></div></>}
    </main>
    {settingsOpen && connected && <div className="overlay" onMouseDown={e=>{if(e.target===e.currentTarget)setSettingsOpen(false)}}><section className="modal" role="dialog" aria-modal="true" aria-labelledby="settings-title"><div className="modal-head"><div><span className="eyebrow">CẤU HÌNH</span><h2 id="settings-title">Model của bạn</h2></div><button className="icon-button" onClick={()=>setSettingsOpen(false)} aria-label="Đóng"><X size={18}/></button></div><p>Chọn cấu hình đã lưu trên server hoặc nhập thông tin model cho phiên hiện tại.</p><div className="tabs"><button className={source==='stored'?'active':''} onClick={()=>setSource('stored')}>Đã lưu trên server</button><button className={source==='mobile_ephemeral'?'active':''} onClick={()=>setSource('mobile_ephemeral')}>Cấu hình tạm</button></div>{source==='stored' ? <><label htmlFor="provider">PROVIDER ID</label><input id="provider" value={provider} onChange={e=>setProvider(e.target.value)} placeholder="Ví dụ: openai"/><label htmlFor="keyid">KEY ID <span>KHÔNG BẮT BUỘC</span></label><input id="keyid" value={keyID} onChange={e=>setKeyID(e.target.value)} placeholder="Để trống nếu không cần"/></> : <><label htmlFor="type">PROVIDER</label><select id="type" value={providerType} onChange={e=>setProviderType(e.target.value)}><option value="openai">OpenAI Chat</option><option value="openai_responses">OpenAI Responses</option><option value="anthropic">Anthropic</option><option value="google">Google Gemini</option></select><label htmlFor="url">BASE URL <span>KHÔNG BẮT BUỘC</span></label><input id="url" value={baseURL} onChange={e=>setBaseURL(e.target.value)} placeholder="Mặc định của provider"/><label htmlFor="key">API KEY</label><input id="key" type="password" value={apiKey} onChange={e=>setApiKey(e.target.value)} autoComplete="off" placeholder="Chỉ lưu trong bộ nhớ trang"/></>}<label htmlFor="model">MODEL ID</label><input id="model" value={model} onChange={e=>setModel(e.target.value)} placeholder="Ví dụ: gpt-4.1"/><div className="modal-foot"><span>Cấu hình tạm sẽ mất khi tải lại trang.</span><Button onClick={()=>setSettingsOpen(false)}><Check size={15}/> Xong</Button></div></section></div>}
  </div>
}
createRoot(document.getElementById('root')).render(<App />)
