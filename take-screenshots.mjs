#!/usr/bin/env node
// Puppeteer screenshot generator for botmux README
// Uses evaluateOnNewDocument to monkey-patch fetch with mock data
// Usage: npx puppeteer take-screenshots.mjs

import puppeteer from 'puppeteer';
import { join, dirname } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SCREENSHOTS_DIR = join(__dirname, 'screenshots');
const BASE_URL = 'http://localhost:8081';
const VIEWPORT = { width: 1280, height: 720 };

// Mock data for all API endpoints
const MOCK_DATA = {
  currentUser: {
    id: 1, username: 'admin', display_name: 'Admin',
    role: 'admin', must_change_password: false
  },
  authUsers: [
    { id: 1, username: 'admin', display_name: 'Admin', role: 'admin', bot_ids: [1, 2] },
    { id: 2, username: 'operator', display_name: 'Alex Operator', role: 'user', bot_ids: [1] },
    { id: 3, username: 'viewer', display_name: 'Jane Viewer', role: 'user', bot_ids: [2] },
  ],
  apiKeys: [
    { id: 1, name: 'CI/CD Pipeline', user_id: 1, enabled: true, last_used: '2026-03-08T14:20:00Z', created_at: '2026-02-15T10:00:00Z' },
    { id: 2, name: 'Monitoring', user_id: 2, enabled: true, last_used: null, created_at: '2026-03-01T08:00:00Z' },
    { id: 3, name: 'Legacy Integration', user_id: 1, enabled: false, last_used: '2026-01-20T09:15:00Z', created_at: '2026-01-10T12:00:00Z' },
  ],
  bots: [
    {
      id: 1, name: 'shopbot', token: '123456:FAKE',
      bot_username: 'shopbot',
      source: 'cli', manage_enabled: true, proxy_enabled: true,
      running: true, backend_url: 'https://shop.example.com/webhook',
      backend_status: 'ok:200', backend_checked_at: '2026-03-08T07:30:44Z',
      updates_forwarded: 142, offset: 100000000, polling_timeout: 30,
      last_activity: '2026-03-08T10:15:29Z', last_error: '',
      secret_token: '', description: 'Handles shop orders, product queries and customer support'
    },
    {
      id: 2, name: 'alertbot', token: '654321:FAKE',
      bot_username: 'alertbot',
      source: 'web', manage_enabled: true, proxy_enabled: false,
      running: true, backend_url: '',
      backend_status: '', backend_checked_at: '',
      updates_forwarded: 0, offset: 200000000, polling_timeout: 30,
      last_activity: '2026-03-08T09:55:12Z', last_error: '',
      secret_token: '', description: 'Sends alerts and notifications'
    }
  ],
  chats: [
    {
      id: -1001234567890, bot_id: 1, title: 'My Group Chat', type: 'supergroup',
      username: 'mygroupchat', member_count: 47, description: 'A friendly community chat for developers and tech enthusiasts.',
      is_admin: true, updated_at: '2026-03-08T10:15:29Z'
    },
    {
      id: -1009876543210, bot_id: 1, title: 'Private: 5551234', type: 'private',
      username: '', member_count: 2, description: '',
      is_admin: false, updated_at: '2026-03-08T09:40:00Z'
    }
  ],
  messages: (() => {
    const users = [
      { name: '@bob_user', id: 1001 },
      { name: '@charlie_x', id: 1002 },
      { name: '@dave_pilot', id: 1003 },
      { name: '@eve_admin', id: 1004 },
      { name: '@frank_mod', id: 1005 },
      { name: '@grace_ann', id: 1006 },
    ];
    const texts = [
      'Hey everyone, whats up?',
      'Just finished the project proposal, check it out!',
      'Has anyone seen the new update? Looks great',
      'Meeting at 3pm today, dont forget',
      'Can someone help me with the API docs?',
      'Sure, Ill send you the link',
      'Thanks! That was super helpful',
      'Good morning everyone!',
      'The deploy went smooth, no issues',
      'Anyone up for coffee after work?',
      'I pushed the fix to staging',
      'Looks like the tests are passing now',
    ];
    const msgs = [];
    for (let i = 0; i < 12; i++) {
      const u = users[i % users.length];
      const h = 14 + Math.floor(i / 3);
      const m = 10 + (i * 7) % 50;
      msgs.push({
        id: 39550 + i, chat_id: -1001234567890,
        from_user: u.name, from_id: u.id, text: texts[i],
        date: 1741356600 + i * 600,
        date_str: `2026-03-07 ${String(h).padStart(2,'0')}:${String(m).padStart(2,'0')}:00`,
        deleted: i === 4, media_type: '', file_id: '',
        reply_to_message_id: i === 5 ? 39554 : 0
      });
    }
    return msgs.reverse();
  })(),
  stats: {
    chat_id: -1001234567890, title: 'My Group Chat',
    total_messages: 1247,
    today_messages: 38,
    active_users: 12,
    hourly_stats: Array.from({length: 24}, (_, h) => ({
      hour: h, count: [2,1,0,0,0,1,3,8,15,22,18,14,19,25,21,17,12,9,14,11,8,5,4,3][h]
    })),
    top_users: [
      { user_id: 1001, username: '@bob_user', count: 186 },
      { user_id: 1004, username: '@eve_admin', count: 143 },
      { user_id: 1003, username: '@dave_pilot', count: 112 },
      { user_id: 1005, username: '@frank_mod', count: 98 },
      { user_id: 1002, username: '@charlie_x', count: 87 },
    ]
  },
  admins: [
    {
      user_id: 1001, username: '@shopbot',
      status: 'administrator', custom_title: '',
      can_manage_chat: true, can_delete_messages: true, can_restrict_members: true,
      can_invite_users: true, can_pin_messages: true, can_change_info: true,
      can_promote_members: true
    },
    {
      user_id: 1005, username: '@frank_mod',
      status: 'administrator', custom_title: 'Admin',
      can_manage_chat: true, can_delete_messages: true, can_restrict_members: true,
      can_invite_users: true, can_pin_messages: true, can_change_info: false,
      can_promote_members: false
    },
    {
      user_id: 1007, username: '@hank_star',
      status: 'creator', custom_title: '',
      can_manage_chat: true, can_delete_messages: true, can_restrict_members: true,
      can_invite_users: true, can_pin_messages: true, can_change_info: true,
      can_promote_members: true
    },
    {
      user_id: 1008, username: '@alice_dev',
      status: 'administrator', custom_title: 'mod',
      can_manage_chat: true, can_delete_messages: true, can_restrict_members: true,
      can_invite_users: true, can_pin_messages: true, can_change_info: true,
      can_promote_members: false
    },
  ],
  users: [
    { user_id: 29214101, username: '@eve_admin', message_count: 143, last_seen: '2026-03-07 18:21:00', tags: [] },
    { user_id: 29225212, username: '@bob_user', message_count: 186, last_seen: '2026-03-08 08:04:42', tags: [] },
    { user_id: 29236323, username: '@dave_pilot', message_count: 112, last_seen: '2026-03-07 19:38:52', tags: [] },
    { user_id: 40721942, username: '@ivan_play', message_count: 54, last_seen: '2026-03-07 15:36:37', tags: [] },
    { user_id: 48430101, username: '@hank_star', message_count: 67, last_seen: '2026-03-07 11:56:14', tags: [] },
    { user_id: 11234567, username: '@alice_dev', message_count: 98, last_seen: '2026-03-07 15:35:05', tags: [] },
    { user_id: 13648440, username: '@frank_mod', message_count: 98, last_seen: '2026-03-08 09:12:33', tags: [] },
    { user_id: 87654321, username: '@charlie_x', message_count: 87, last_seen: '2026-03-07 22:15:41', tags: [] },
    { user_id: 55512340, username: '@grace_ann', message_count: 42, last_seen: '2026-03-07 20:05:18', tags: [] },
  ],
  tags: [
    { id: 1, chat_id: -1001234567890, user_id: 100200300, username: 'bob_user', tag: 'BOT', color: '#06b6d4' }
  ],
  adminlog: [
    {
      id: 1, chat_id: -1001234567890, action: 'ADD TAG',
      actor_name: 'Bot (@shopbot)', target_id: 100200300, target_name: 'User 100200300',
      details: 'Tag: bot', created_at: '2026-03-08T07:01:28Z'
    },
    {
      id: 2, chat_id: -1001234567890, action: 'DEL MSG',
      actor_name: 'Bot (@shopbot)', target_id: 0, target_name: '',
      details: 'Message ID: 39550', created_at: '2026-03-07T15:48:11Z'
    },
    {
      id: 3, chat_id: -1001234567890, action: 'BAN',
      actor_name: 'Bot (@shopbot)', target_id: 99999, target_name: '@spammer99',
      details: 'Banned user', created_at: '2026-03-07T12:22:05Z'
    },
    {
      id: 4, chat_id: -1001234567890, action: 'PIN',
      actor_name: 'Bot (@shopbot)', target_id: 0, target_name: '',
      details: 'Message ID: 39545', created_at: '2026-03-06T18:10:33Z'
    },
  ],
  routes: [
    {
      id: 1, source_bot_id: 1, target_bot_id: 2,
      condition_type: 'text', condition_value: '(?i)alert|urgent|down',
      action: 'forward', target_chat_id: -1009876543210,
      enabled: true, description: 'Forward alerts to alertbot'
    },
    {
      id: 2, source_bot_id: 1, target_bot_id: 2,
      condition_type: 'user_id', condition_value: '1003',
      action: 'copy', target_chat_id: 0,
      enabled: true, description: 'Copy messages from dave_pilot'
    },
    {
      id: 3, source_bot_id: 1, target_bot_id: 2,
      condition_type: 'text', condition_value: 'spam|crypto|airdrop',
      action: 'drop', target_chat_id: 0,
      enabled: false, description: 'Drop spam messages'
    },
  ],
  llmConfig: {
    api_url: '', api_key: '', model: '', system_prompt: '', enabled: false
  },
  botDescription: '',
  bridges: [
    {
      id: 1, bot_id: 1, protocol: 'slack', name: 'Team Slack Bridge',
      config: '{"bot_token":"xoxb-fake","channel_id":"C01EXAMPLE","signing_secret":"***"}',
      enabled: true, created_at: '2026-03-01T10:00:00Z',
      last_activity: '2026-03-08T09:30:00Z', last_error: ''
    }
  ]
};

function setupMockFetch(page, { authEnabled }) {
  const mockDataStr = JSON.stringify(MOCK_DATA);
  return page.evaluateOnNewDocument((mockDataJSON, baseUrl, withAuth) => {
    const MOCK = JSON.parse(mockDataJSON);

    function mockResponse(url, init) {
      let u;
      try { u = new URL(url, baseUrl); } catch { return null; }
      const path = u.pathname;
      const params = u.searchParams;

      // Auth endpoints
      if (path === '/api/auth/me') {
        if (!withAuth) return '__401__';
        return MOCK.currentUser;
      }
      if (path === '/api/auth/login') return { user: MOCK.currentUser };
      if (path === '/api/auth/users') return MOCK.authUsers;
      if (path === '/api/auth/api-keys') return MOCK.apiKeys;

      // Data endpoints
      if (path === '/api/bots') return MOCK.bots;
      if (path === '/api/bots/validate') return { ok: true, username: 'newbot', first_name: 'New Bot' };
      if (path === '/api/bots/description') return { description: MOCK.botDescription };
      if (path === '/api/chats') return MOCK.chats;
      if (path === '/api/messages') return MOCK.messages;
      if (path === '/api/messages/search') return MOCK.messages.filter(m => m.text.toLowerCase().includes((params.get('q') || '').toLowerCase()));
      if (path === '/api/stats') return MOCK.stats;
      if (path === '/api/admins') return MOCK.admins;
      if (path === '/api/users/list') return MOCK.users;
      if (path === '/api/tags') return MOCK.tags;
      if (path === '/api/tags/user') return [];
      if (path === '/api/adminlog') return MOCK.adminlog;
      if (path === '/api/routes') return MOCK.routes;
      if (path === '/api/chats/refresh') return MOCK.chats[0];
      if (path === '/api/llm-config') return MOCK.llmConfig;
      if (path === '/api/bridges') return MOCK.bridges;
      if (path === '/api/health') return { status: 'ok' };
      if (path === '/api/bots/health') return { status: 'ok', latency_ms: 42 };
      // Catch-all for any unhandled /api/ endpoint — prevent 401 from real server
      return [];
    }

    const origFetch = window.fetch;
    window.fetch = function(input, init) {
      const url = typeof input === 'string' ? input : input.url;
      if (url.includes('/api/')) {
        const data = mockResponse(url, init);
        if (data === '__401__') {
          return Promise.resolve(new Response(JSON.stringify({error: 'unauthorized'}), {
            status: 401,
            headers: { 'Content-Type': 'application/json' }
          }));
        }
        if (data !== null) {
          return Promise.resolve(new Response(JSON.stringify(data), {
            status: 200,
            headers: { 'Content-Type': 'application/json' }
          }));
        }
      }
      return origFetch.apply(this, arguments);
    };
  }, mockDataStr, BASE_URL, authEnabled);
}

async function takeScreenshots() {
  const browser = await puppeteer.launch({
    headless: true,
    args: ['--no-sandbox', '--disable-setuid-sandbox']
  });

  // ===== LOGIN SCREEN =====
  console.log('Taking login screenshot...');
  const loginPage = await browser.newPage();
  await loginPage.setViewport(VIEWPORT);
  await setupMockFetch(loginPage, { authEnabled: false });
  await loginPage.evaluateOnNewDocument(() => {
    localStorage.setItem('lang', 'en');
    localStorage.removeItem('theme');
  });
  await loginPage.goto(BASE_URL, { waitUntil: 'networkidle0', timeout: 10000 });
  await sleep(500);
  await loginPage.screenshot({ path: join(SCREENSHOTS_DIR, '01-login.png') });
  console.log('  01-login.png');
  await loginPage.close();

  // ===== MAIN APP =====
  const page = await browser.newPage();
  await page.setViewport(VIEWPORT);
  await setupMockFetch(page, { authEnabled: true });
  await page.evaluateOnNewDocument(() => {
    localStorage.setItem('lang', 'en');
    localStorage.removeItem('theme');
  });
  await page.goto(BASE_URL, { waitUntil: 'networkidle0', timeout: 10000 });
  await sleep(500);

  // 02 - Dashboard (bot list, no bot selected)
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '02-dashboard.png') });
  console.log('  02-dashboard.png');

  // Click the first bot to show detail
  await page.evaluate(() => document.querySelector('.bot-item')?.click());
  await sleep(500);

  // 03 - Bot Detail (with routing rules)
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '03-bot-detail.png') });
  console.log('  03-bot-detail.png');

  // Click the first chat "My Group Chat"
  await page.evaluate(() => {
    const item = document.querySelector('.chat-item');
    if (item) item.click();
  });
  await sleep(500);

  // 04 - Messages
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '04-messages.png') });
  console.log('  04-messages.png');

  // Click Analytics tab
  await page.evaluate(() => document.querySelector('.tab[data-tab="stats"]')?.click());
  await sleep(500);

  // 05 - Analytics
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '05-analytics.png') });
  console.log('  05-analytics.png');

  // Click Admins tab
  await page.evaluate(() => document.querySelector('.tab[data-tab="admins"]')?.click());
  await sleep(500);

  // 06 - Admins
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '06-admins.png') });
  console.log('  06-admins.png');

  // Click Users tab
  await page.evaluate(() => document.querySelector('.tab[data-tab="users"]')?.click());
  await sleep(500);

  // 07 - Users
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '07-users.png') });
  console.log('  07-users.png');

  // Click Tags tab
  await page.evaluate(() => document.querySelector('.tab[data-tab="tags"]')?.click());
  await sleep(500);

  // 08 - Tags
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '08-tags.png') });
  console.log('  08-tags.png');

  // Click Audit Log tab
  await page.evaluate(() => document.querySelector('.tab[data-tab="adminlog"]')?.click());
  await sleep(500);

  // 09 - Audit Log
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '09-audit-log.png') });
  console.log('  09-audit-log.png');

  // Open Add Bot modal
  await page.evaluate(() => showBotModal());
  await sleep(400);

  // 10 - Add Bot modal
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '10-add-bot.png') });
  console.log('  10-add-bot.png');

  // Close Add Bot modal
  await page.evaluate(() => closeBotModal());
  await sleep(200);

  // Open User Management modal
  await page.evaluate(() => showUsersModal());
  await sleep(400);

  // 11 - User Management
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '11-user-management.png') });
  console.log('  11-user-management.png');

  // Close Users modal
  await page.evaluate(() => closeUsersModal());
  await sleep(200);

  // Open API Keys modal
  await page.evaluate(() => showAPIKeysModal());
  await sleep(400);

  // 12 - API Keys
  await page.screenshot({ path: join(SCREENSHOTS_DIR, '12-api-keys.png') });
  console.log('  12-api-keys.png');

  await browser.close();
  console.log('\nAll screenshots generated!');
}

function sleep(ms) {
  return new Promise(r => setTimeout(r, ms));
}

takeScreenshots().catch(err => {
  console.error(err);
  process.exit(1);
});
