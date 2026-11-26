import { expect, test, type Page } from '@playwright/test'
import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { once } from 'node:events'
import { mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs'
import { createServer, request, type Server } from 'node:http'
import { createServer as createHTTPS } from 'node:https'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const password = 'test-workspace-password'
const replyText = 'Deterministic streamed answer'

async function listen(server: Server): Promise<string> {
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  return `127.0.0.1:${(server.address() as AddressInfo).port}`
}

async function close(server: Server) {
  server.close()
  server.closeAllConnections()
  await once(server, 'close')
}

test('opens the passwordless local workspace and preserves authenticated history over HTTPS', async ({
  page,
  context,
  isMobile,
}) => {
  test.setTimeout(120_000)
  const dir = mkdtempSync(join(realpathSync(tmpdir()), 'generative-e2e-'))
  const binary = join(dir, 'generative')
  const data = join(dir, 'data')
  const identity = join(dir, 'identity')
  const socket = join(identity, 'control.sock')
  const store = join(identity, 'web-auth.json')
  const authArgs = ['web', 'password', 'set', '--ipc', socket, '--stdin']
  const upstreamPlaceholder = createServer()
  const upstream = await listen(upstreamPlaceholder)
  await close(upstreamPlaceholder)

  let daemon: ChildProcess | undefined
  let daemonLog = ''
  let provider: Server | undefined
  let proxy: Server | undefined
  let finishGeneration: (() => void) | undefined
  let cancelReachedProvider = false
  let generationCalls = 0

  async function stopDaemon() {
    if (!daemon || daemon.exitCode !== null || daemon.signalCode !== null) return
    daemon.kill('SIGTERM')
    await once(daemon, 'exit')
  }

  try {
    execFileSync('go', ['build', '-o', binary, './cmd/generative'], { cwd: '..' })
    execFileSync(
      'openssl',
      [
        'req',
        '-x509',
        '-newkey',
        'rsa:2048',
        '-nodes',
        '-days',
        '1',
        '-subj',
        '/CN=127.0.0.1',
        '-keyout',
        join(dir, 'tls.key'),
        '-out',
        join(dir, 'tls.crt'),
      ],
      { stdio: 'ignore' },
    )
    provider = createServer((incoming, response) => {
      incoming.resume()
      incoming.on('end', () => {
        const call = ++generationCalls
        response.writeHead(200, { 'Content-Type': 'text/event-stream' })
        const text = call === 1 ? replyText : 'Partial answer to cancel'
        response.write(
          `data: ${JSON.stringify({
            id: 'fake-answer',
            object: 'chat.completion.chunk',
            choices: [{ index: 0, delta: { content: text } }],
          })}\n\n`,
        )
        if (call === 1) {
          finishGeneration = () => response.end('data: [DONE]\n\n')
        } else {
          response.once('close', () => {
            cancelReachedProvider = true
          })
        }
      })
    })
    const providerAddr = await listen(provider)
    proxy = createHTTPS(
      {
        key: readFileSync(join(dir, 'tls.key')),
        cert: readFileSync(join(dir, 'tls.crt')),
      },
      (incoming, outgoing) => {
        const back = request(
          `http://${upstream}${incoming.url}`,
          {
            method: incoming.method,
            headers: { ...incoming.headers, 'x-forwarded-proto': 'https' },
          },
          (response) => {
            outgoing.writeHead(response.statusCode ?? 502, response.headers)
            outgoing.flushHeaders()
            response.pipe(outgoing)
          },
        )
        back.on('error', () => {
          if (!outgoing.headersSent) outgoing.writeHead(502)
          outgoing.end()
        })
        outgoing.on('close', () => back.destroy())
        incoming.pipe(back)
      },
    )
    const origin = `https://${await listen(proxy)}`

    async function startDaemon(local = false) {
      daemonLog = ''
      const env = { ...process.env }
      delete env.GENERATIVE_WEB_PASSWORD
      if (!local) env.GENERATIVE_WEB_PASSWORD = password
      daemon = spawn(
        binary,
        [
          'serve',
          '--tcp-listen',
          '127.0.0.1:0',
          '--http-listen',
          upstream,
          ...(local ? [] : ['--web-origin', origin, '--web-trusted-proxy', '127.0.0.1']),
          '--data-dir',
          data,
          '--identity-dir',
          identity,
          '--allow-local-api',
        ],
        { stdio: ['ignore', 'pipe', 'pipe'], env },
      )
      daemon.stdout!.on('data', (chunk) => {
        daemonLog += String(chunk)
      })
      daemon.stderr!.on('data', (chunk) => {
        daemonLog += String(chunk)
      })
      await expect
        .poll(
          async () => {
            try {
              return (
                await page.request.get((local ? `http://${upstream}` : origin) + '/web/auth/status')
              ).status()
            } catch {
              return 0
            }
          },
          { timeout: 10_000 },
        )
        .toBe(200)
    }

    const composer = page.getByLabel('Nhắn cho Generative…')
    const passwordInput = page.getByLabel('Mật khẩu')
    async function login() {
      await passwordInput.fill(password)
      await page.getByRole('button', { name: 'Đăng nhập' }).click()
      await expect(composer).toBeVisible()
    }

    async function sidebar(target: Page) {
      if (isMobile) await target.getByRole('button', { name: 'Mở menu' }).click()
    }

    await startDaemon(true)
    await expect.poll(() => daemonLog).toContain('WARNING: Web UI has no password.')
    await page.goto(`http://${upstream}/`)
    await expect(composer).toBeVisible()
    await expect(passwordInput).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'Đăng xuất' })).toHaveCount(0)
    await expect(
      page.getByText('Bất kỳ ai truy cập được địa chỉ web này đều có thể mở không gian làm việc.'),
    ).toBeVisible()
    execFileSync(binary, authArgs, { input: 'separate-saved-password\n' })
    await expect(passwordInput).toBeVisible({ timeout: 12_000 })
    await stopDaemon()
    await startDaemon()
    expect(daemonLog).not.toContain('WARNING: Web UI has no password.')
    const bypass = await page.request.get(`http://${upstream}/web/auth/status`, {
      headers: { Host: new URL(origin).host },
    })
    expect(bypass.status()).toBe(403)
    await page.goto(origin)
    await login()
    const cookie = (await context.cookies(origin)).find((item) => item.name === '__Host-generative-web')
    expect(cookie).toMatchObject({ secure: true, httpOnly: true, sameSite: 'Strict', path: '/' })

    await page.reload()
    await expect(composer).toBeVisible()
    await page.getByRole('button', { name: 'Chưa chọn model' }).click()
    await page.getByRole('tab', { name: 'Cấu hình tạm' }).click()
    await page.locator('#ephemeral-model').fill('test-model')
    await page.locator('#base-url').fill(`http://${providerAddr}/v1`)
    await page.locator('#api-key').fill('fake-key')
    await page.getByRole('button', { name: 'Áp dụng' }).click()
    await composer.fill('First durable prompt')
    await page.getByRole('button', { name: 'Gửi' }).click()
    await expect(page.getByText(/^Deterministic streamed/)).toBeVisible()
    await expect(page.getByRole('button', { name: 'Dừng tạo' })).toBeVisible()
    finishGeneration?.()
    await expect(page.getByRole('button', { name: 'Dừng tạo' })).toHaveCount(0)

    await composer.fill('Cancel this prompt')
    await page.getByRole('button', { name: 'Gửi' }).click()
    await expect(page.getByText(/^Partial answer/)).toBeVisible()
    await page.getByRole('button', { name: 'Dừng tạo' }).click()
    await expect.poll(() => cancelReachedProvider).toBe(true)
    await expect(page.getByRole('button', { name: 'Dừng tạo' })).toHaveCount(0)

    const tab = await context.newPage()
    await tab.goto(origin)
    await expect(tab.getByLabel('Nhắn cho Generative…')).toBeVisible({ timeout: 12_000 })
    await composer.fill('draft cleared on logout')
    await sidebar(page)
    await page.getByRole('button', { name: 'Đăng xuất' }).click()
    const confirm = page.getByRole('alertdialog')
    await confirm.getByRole('button', { name: 'Đăng xuất' }).click()
    await expect(passwordInput).toBeVisible()
    await expect(tab.getByLabel('Mật khẩu')).toBeVisible({ timeout: 12_000 })
    await login()
    await expect(tab.getByLabel('Nhắn cho Generative…')).toBeVisible({ timeout: 12_000 })
    await expect(composer).toHaveValue('')
    await sidebar(page)
    await page.locator(isMobile ? '.mobile-sidebar .chat-item-title' : 'aside .chat-item-title').click()
    await expect(page.getByText(replyText, { exact: true })).toBeVisible()

    await stopDaemon()
    await expect(page.locator('.connection-banner')).toBeVisible({ timeout: 12_000 })
    await expect(composer).toBeVisible()
    await startDaemon()
    await expect(page.locator('.connection-banner')).toHaveCount(0, { timeout: 12_000 })
    await page.reload()
    await expect(composer).toBeVisible()
    await sidebar(page)
    await page.locator(isMobile ? '.mobile-sidebar .chat-item-title' : 'aside .chat-item-title').click()
    await expect(page.getByText(replyText, { exact: true })).toBeVisible()

    await stopDaemon()
    const state = JSON.parse(readFileSync(store, 'utf8')) as { sessions: Record<string, { expires: string }> }
    for (const session of Object.values(state.sessions)) session.expires = new Date(0).toISOString()
    writeFileSync(store, JSON.stringify(state), { mode: 0o600 })
    await startDaemon()
    await expect(passwordInput).toBeVisible({ timeout: 12_000 })
    await expect(page.getByText('Phiên đã hết hạn', { exact: true })).toBeVisible()
  } catch (error) {
    test
      .info()
      .attach('daemon log', { body: daemonLog, contentType: 'text/plain' })
      .catch(() => {})
    throw error
  } finally {
    await stopDaemon()
    if (proxy) await close(proxy)
    if (provider) await close(provider)
    rmSync(dir, { recursive: true, force: true })
  }
})
