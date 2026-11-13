'use strict';
/*
 * mesh 单聊天框前端。纯手写，无框架、无构建、无 CDN。
 *
 * 三条硬规矩（改代码时别破）：
 *   1. 不用 innerHTML 塞任何来路不明的字符串 —— 一律 createElement + textContent。
 *   2. 不用 eval / new Function。
 *   3. 金额未知就显示"未知"，绝不显示 0。用户对花钱极度敏感。
 */

/* ------------------------------------------------------------------ *
 * 常量
 * ------------------------------------------------------------------ */

var RECONNECT_MIN_MS = 800;
var RECONNECT_MAX_MS = 15000;
var SNAPSHOT_DEBOUNCE_MS = 120;

var UNKNOWN = '未知';

/* ------------------------------------------------------------------ *
 * 小工具：DOM 构造（唯一允许写 DOM 的入口）
 * ------------------------------------------------------------------ */

function el(tag, className, text) {
  var n = document.createElement(tag);
  if (className) { n.className = className; }
  if (text !== undefined && text !== null) { n.textContent = String(text); }
  return n;
}

function clear(node) {
  while (node && node.firstChild) { node.removeChild(node.firstChild); }
}

function fromTemplate(id) {
  var tpl = document.getElementById(id);
  if (!tpl) { return null; }
  return tpl.content.firstElementChild.cloneNode(true);
}

function $(id) { return document.getElementById(id); }

function show(node, visible) {
  if (!node) { return; }
  if (visible) { node.removeAttribute('hidden'); } else { node.setAttribute('hidden', ''); }
}

/* ------------------------------------------------------------------ *
 * 格式化
 * ------------------------------------------------------------------ */

function fmtAmount(currency, micros) {
  // micros 是 1e-6 货币单位。空货币或非法金额一律按未知处理。
  if (typeof micros !== 'number' || !isFinite(micros) || micros < 0) { return UNKNOWN; }
  var unit = 1000000;
  var whole = Math.floor(micros / unit);
  var frac = micros % unit;
  var s = String(whole) + '.' + String(Math.round(frac / 10000)).padStart(2, '0').slice(0, 2);
  if (!currency) { return s; }
  return s + ' ' + currency;
}

// 一组 {currency, amountMicros} 的展示。绝不跨币种相加。
function fmtMoneyList(list) {
  if (!list || !list.length) { return UNKNOWN; }
  var parts = [];
  for (var i = 0; i < list.length; i++) {
    parts.push(fmtAmount(list[i].currency, list[i].amountMicros));
  }
  return parts.join(' + ');
}

function fmtTime(ms) {
  if (!ms) { return ''; }
  var d = new Date(ms);
  if (isNaN(d.getTime())) { return ''; }
  var hh = String(d.getHours()).padStart(2, '0');
  var mm = String(d.getMinutes()).padStart(2, '0');
  return hh + ':' + mm;
}

function fmtCount(n) {
  if (typeof n !== 'number' || !isFinite(n)) { return UNKNOWN; }
  return String(n);
}

var STATE_TEXT = {
  queued: '排队中',
  executing: '执行中',
  reviewing: '审查中',
  paused: '已暂停',
  waiting_user: '等你决定',
  completed: '已完成',
  failed: '失败',
  cancelled: '已取消'
};

function stateText(s) {
  if (!s) { return UNKNOWN; }
  return STATE_TEXT[s] || s;
}

var BOT_STATUS_TEXT = {
  idle: '空闲',
  thinking: '思考中',
  browser: '在操作浏览器',
  awaiting_auth: '等你授权',
  error: '出错'
};

function botStatusText(s) {
  if (!s) { return UNKNOWN; }
  return BOT_STATUS_TEXT[s] || s;
}

var AUTHOR_KIND_TEXT = {
  user: '你',
  bot: 'bot',
  system: '系统'
};

var MSG_KIND_TEXT = {
  chat: '聊天',
  task: '任务',
  result: '结果',
  handoff: '交接',
  notice: '通知'
};

/* ------------------------------------------------------------------ *
 * 全局状态
 * ------------------------------------------------------------------ */

var S = {
  csrf: '',
  roomId: '',
  snapshot: null,
  messages: [],          // 已渲染消息 id，用于去重
  seenMsgIds: Object.create(null),
  cursor: 0,
  cost: null,
  costDim: 'bot',
  es: null,
  reconnectDelay: RECONNECT_MIN_MS,
  snapshotTimer: 0,
  streamingMsgId: '',
  currentWorkId: ''
};

/* ------------------------------------------------------------------ *
 * HTTP
 * ------------------------------------------------------------------ */

function apiURL(path) { return path; }

function getJSON(path) {
  return fetch(apiURL(path), {
    method: 'GET',
    credentials: 'same-origin',
    headers: { 'Accept': 'application/json' }
  }).then(function (r) {
    if (!r.ok) { throw new Error('HTTP ' + r.status); }
    return r.json();
  });
}

function postJSON(path, body) {
  return fetch(apiURL(path), {
    method: 'POST',
    credentials: 'same-origin',
    headers: {
      'Content-Type': 'application/json',
      'Accept': 'application/json',
      // CSRF：服务端下发的令牌，放在自定义头里。跨站表单无法伪造这个头。
      'X-Mesh-CSRF': S.csrf
    },
    body: JSON.stringify(body || {})
  }).then(function (r) {
    return r.text().then(function (txt) {
      var data = null;
      if (txt) {
        try { data = JSON.parse(txt); } catch (e) { data = null; }
      }
      if (!r.ok) {
        var msg = (data && (data.error || data.message)) || ('HTTP ' + r.status);
        throw new Error(msg);
      }
      return data;
    });
  });
}

/* ------------------------------------------------------------------ *
 * 快照刷新
 * ------------------------------------------------------------------ */

function loadSnapshot() {
  return getJSON('/api/state').then(function (snap) {
    applySnapshot(snap);
  }).catch(function (e) {
    setConn('error', '读取状态失败：' + e.message);
  });
}

function scheduleSnapshot() {
  if (S.snapshotTimer) { return; }
  S.snapshotTimer = window.setTimeout(function () {
    S.snapshotTimer = 0;
    loadSnapshot();
  }, SNAPSHOT_DEBOUNCE_MS);
}

function applySnapshot(snap) {
  if (!snap || typeof snap !== 'object') { return; }
  S.snapshot = snap;
  if (snap.session && snap.session.csrf) { S.csrf = snap.session.csrf; }
  if (snap.session && snap.session.roomId) { S.roomId = snap.session.roomId; }

  renderWork(snap.current);
  renderBots(snap.bots || []);
  renderDecisions(snap.pending || []);
  renderCapabilities(snap.capabilities || []);
  renderNotices(snap.notices || []);
  renderRoleOptions(snap.bots || []);
  renderMessages(snap.messages || []);
  loadCost();
}

/* ------------------------------------------------------------------ *
 * 顶部连接状态
 * ------------------------------------------------------------------ */

function setConn(state, text) {
  var box = $('conn');
  var label = $('conn-text');
  if (box) { box.setAttribute('data-state', state); }
  if (label) { label.textContent = text; }
}

/* ------------------------------------------------------------------ *
 * 当前 Work 条
 * ------------------------------------------------------------------ */

function renderWork(cur) {
  var goal = $('work-goal');
  var stateChip = $('work-state');
  var exName = $('work-executor');
  var exVer = $('work-executor-ver');
  var rvName = $('work-reviewer');
  var rvVer = $('work-reviewer-ver');
  var rounds = $('work-rounds');

  if (!cur || !cur.workId) {
    S.currentWorkId = '';
    if (goal) { goal.textContent = '尚未有进行中的委托'; }
    if (stateChip) {
      stateChip.textContent = '空闲';
      stateChip.setAttribute('data-state', '');
    }
    if (exName) { exName.textContent = '未指定'; }
    if (rvName) { rvName.textContent = '未指定'; }
    if (exVer) { exVer.textContent = ''; }
    if (rvVer) { rvVer.textContent = ''; }
    if (rounds) { rounds.textContent = ''; }
    renderStopFlag(null);
    setControlsEnabled(false);
    return;
  }

  S.currentWorkId = cur.workId;
  if (goal) { goal.textContent = cur.goal || '（未填写目标）'; }
  if (stateChip) {
    stateChip.textContent = stateText(cur.state);
    stateChip.setAttribute('data-state', cur.state || '');
  }
  if (exName) { exName.textContent = (cur.executor && cur.executor.name) || '未指定'; }
  if (rvName) { rvName.textContent = (cur.reviewer && cur.reviewer.name) || '未指定'; }
  // 分工版本号：让用户能看出"这条消息是在第几版分工下产生的"
  if (exVer) { exVer.textContent = cur.executor && cur.executor.version ? 'v' + cur.executor.version : ''; }
  if (rvVer) { rvVer.textContent = cur.reviewer && cur.reviewer.version ? 'v' + cur.reviewer.version : ''; }
  if (rounds) {
    var max = typeof cur.maxRounds === 'number' ? cur.maxRounds : 0;
    var used = typeof cur.rounds === 'number' ? cur.rounds : 0;
    rounds.textContent = max > 0 ? ('自动审查 ' + used + '/' + max + ' 轮') : '';
  }
  renderStopFlag(cur);
  setControlsEnabled(true);
}

// 三类"未知"之二：停止未确认。
// 只要后端没有给出确定的终止确认，就绝不能显示"已停止"。
function renderStopFlag(cur) {
  var flag = $('flag-stop');
  if (!flag) { return; }
  if (!cur || !cur.stopUnconfirmed) {
    flag.textContent = '';
    show(flag, false);
    return;
  }
  var text = '停止待确认';
  if (cur.stopDetail) { text += '：' + cur.stopDetail; }
  flag.textContent = text;
  show(flag, true);
}

function setControlsEnabled(on) {
  var box = $('decisions');
  if (!box) { return; }
  var btns = box.querySelectorAll('.decision-controls button');
  for (var i = 0; i < btns.length; i++) {
    if (on) { btns[i].removeAttribute('disabled'); } else { btns[i].setAttribute('disabled', ''); }
  }
}

/* ------------------------------------------------------------------ *
 * 待决策
 * ------------------------------------------------------------------ */

function renderDecisions(pending) {
  var block = $('decision-block');
  var list = $('decision-list');
  var count = $('decisions-count');
  if (!list) { return; }

  clear(list);
  if (count) { count.textContent = pending.length ? '(' + pending.length + ')' : ''; }
  show(block, pending.length > 0);

  for (var i = 0; i < pending.length; i++) {
    list.appendChild(buildDecision(pending[i]));
  }
}

function buildDecision(p) {
  var node = fromTemplate('tpl-decision');
  if (!node) { node = el('li', 'decision'); }

  var kind = node.querySelector('.decision-kind');
  var title = node.querySelector('.decision-title');
  var detail = node.querySelector('.decision-detail');
  var actions = node.querySelector('.decision-actions');

  var kindText = p.kind === 'approval' ? '等你授权' : '等你确认';
  if (kind) {
    kind.textContent = kindText;
    kind.setAttribute('data-kind', p.kind || '');
  }
  if (title) { title.textContent = p.title || '需要你决定'; }
  if (detail) {
    detail.textContent = p.detail || '';
    show(detail, !!p.detail);
  }

  if (actions) {
    clear(actions);
    var opts = p.options || [];
    if (p.kind === 'approval' && opts.length) {
      for (var i = 0; i < opts.length; i++) {
        actions.appendChild(makeApprovalButton(p, opts[i]));
      }
    } else {
      actions.appendChild(makeDismissButton(p));
    }
  }
  return node;
}

function makeApprovalButton(p, option) {
  var b = el('button', 'btn-approve', option);
  b.type = 'button';
  b.addEventListener('click', function () {
    b.disabled = true;
    postJSON('/api/work/' + encodeURIComponent(p.workId) + '/control', {
      action: 'approval',
      approvalId: p.approvalId || '',
      option: option
    }).then(function () {
      setControlMsg('已提交：' + option);
      scheduleSnapshot();
    }).catch(function (e) {
      b.disabled = false;
      setControlMsg('提交失败：' + e.message);
    });
  });
  return b;
}

function makeDismissButton(p) {
  var b = el('button', 'btn-dismiss', '知道了');
  b.type = 'button';
  b.addEventListener('click', function () {
    b.disabled = true;
    postJSON('/api/work/' + encodeURIComponent(p.workId) + '/control', {
      action: 'dismiss_pending',
      pendingId: p.id || ''
    }).then(function () {
      scheduleSnapshot();
    }).catch(function (e) {
      b.disabled = false;
      setControlMsg('提交失败：' + e.message);
    });
  });
  return b;
}

function setControlMsg(text) {
  var m = $('control-msg');
  if (m) { m.textContent = text || ''; }
}

/* ------------------------------------------------------------------ *
 * Bot 状态行
 * ------------------------------------------------------------------ */

function renderBots(bots) {
  var list = $('bot-list');
  if (!list) { return; }
  clear(list);
  if (!bots.length) {
    list.appendChild(el('li', 'bot-empty', '还没有接入任何 bot'));
    return;
  }
  for (var i = 0; i < bots.length; i++) {
    list.appendChild(buildBotRow(bots[i]));
  }
}

function buildBotRow(bot) {
  var node = fromTemplate('tpl-bot');
  if (!node) { node = el('li', 'bot'); }
  node.setAttribute('data-status', bot.status || 'idle');

  var name = node.querySelector('.bot-name');
  var adapter = node.querySelector('.bot-adapter');
  var status = node.querySelector('.bot-status');
  var work = node.querySelector('.bot-work');
  var note = node.querySelector('.bot-note');
  var caps = node.querySelector('.bot-caps');
  var capsWrap = node.querySelector('.bot-caps-wrap');

  if (name) { name.textContent = bot.name || bot.id || '(无名)'; }
  if (adapter) { adapter.textContent = bot.adapter || ''; }
  if (status) {
    status.textContent = botStatusText(bot.status);
    status.setAttribute('data-status', bot.status || 'idle');
  }
  if (work) {
    work.textContent = bot.workGoal ? ('当前：' + bot.workGoal) : '';
    show(work, !!bot.workGoal);
  }
  if (note) {
    // 状态细节：例如"在操作浏览器：example.com/checkout"，或出错原因
    note.textContent = bot.statusDetail || '';
    show(note, !!bot.statusDetail);
  }
  if (caps && capsWrap) {
    clear(caps);
    var list = bot.caps || [];
    if (!list.length) {
      show(capsWrap, false);
    } else {
      show(capsWrap, true);
      for (var i = 0; i < list.length; i++) {
        var li = el('li', 'cap-item');
        li.appendChild(el('span', 'cap-label', list[i].label || ''));
        if (list[i].detail) { li.appendChild(el('span', 'cap-detail', list[i].detail)); }
        caps.appendChild(li);
      }
    }
  }
  return node;
}

/* ------------------------------------------------------------------ *
 * 三类"未知"之三：能力受限
 * ------------------------------------------------------------------ */

function renderCapabilities(caps) {
  var list = $('cap-list');
  var flag = $('flag-cap');
  if (!list) { return; }
  clear(list);

  if (!caps.length) {
    list.appendChild(el('li', 'cap-none', '未发现受限能力'));
    show(flag, false);
    return;
  }

  if (flag) {
    flag.textContent = '能力受限 ' + caps.length + ' 项';
    show(flag, true);
  }

  for (var i = 0; i < caps.length; i++) {
    var li = el('li', 'cap-item');
    if (caps[i].scope) {
      li.appendChild(el('span', 'cap-scope', caps[i].scope));
    }
    li.appendChild(el('span', 'cap-label', caps[i].label || ''));
    if (caps[i].detail) {
      li.appendChild(el('span', 'cap-detail', caps[i].detail));
    }
    list.appendChild(li);
  }
}

/* ------------------------------------------------------------------ *
 * 提示
 * ------------------------------------------------------------------ */

function renderNotices(notices) {
  var panel = $('notice-panel');
  var list = $('notice-list');
  if (!list) { return; }
  clear(list);
  show(panel, notices.length > 0);
  for (var i = 0; i < notices.length; i++) {
    var li = el('li', 'notice-item', notices[i].text || '');
    li.setAttribute('data-level', notices[i].level || 'info');
    list.appendChild(li);
  }
}

/* ------------------------------------------------------------------ *
 * 角色下拉
 * ------------------------------------------------------------------ */

function renderRoleOptions(bots) {
  fillSelect($('sel-executor'), bots, true);
  fillSelect($('sel-reviewer'), bots, false);
}

function fillSelect(sel, bots, isExecutor) {
  if (!sel) { return; }
  var keep = sel.value;
  clear(sel);

  var def = el('option', null, '由 agent 建议');
  def.value = '';
  sel.appendChild(def);

  if (!isExecutor) {
    var none = el('option', null, '不审查（由我自己看）');
    none.value = '__none__';
    sel.appendChild(none);
  }

  for (var i = 0; i < bots.length; i++) {
    var o = el('option', null, bots[i].name || bots[i].id);
    o.value = bots[i].id;
    if (bots[i].caps && bots[i].caps.length) {
      // 能力受限的 bot 仍可选，但下拉里就标出来，别让用户事后才发现
      o.textContent = (bots[i].name || bots[i].id) + '（能力受限）';
    }
    sel.appendChild(o);
  }

  // 保留用户当前选择
  var found = false;
  for (var j = 0; j < sel.options.length; j++) {
    if (sel.options[j].value === keep) { found = true; break; }
  }
  sel.value = found ? keep : '';
}

/* ------------------------------------------------------------------ *
 * 消息流
 * ------------------------------------------------------------------ */

function renderMessages(messages) {
  var box = $('messages');
  if (!box) { return; }
  // 首屏按 id 去重后追加；后续增量走 appendMessage
  for (var i = 0; i < messages.length; i++) {
    appendMessage(messages[i]);
  }
  scrollMessages();
}

function appendMessage(m) {
  var box = $('messages');
  if (!box || !m || !m.id) { return; }
  if (S.seenMsgIds[m.id]) { return; }
  S.seenMsgIds[m.id] = true;

  var node = fromTemplate('tpl-msg');
  if (!node) { node = el('article', 'msg'); }

  var author = node.querySelector('.msg-author');
  var kind = node.querySelector('.msg-kind');
  var time = node.querySelector('.msg-time');
  var body = node.querySelector('.msg-body');
  var foot = node.querySelector('.msg-foot');

  node.setAttribute('data-author-kind', m.authorKind || 'bot');
  node.setAttribute('data-kind', m.kind || 'chat');
  node.setAttribute('data-id', m.id);

  if (author) {
    author.textContent = m.author || '未知来源';
    // 版本标签：分工版本或消息版本，二者都有时并排
    var verBits = [];
    if (m.roleVersion) { verBits.push('分工 v' + m.roleVersion); }
    if (m.version) { verBits.push('v' + m.version); }
    if (verBits.length) {
      author.appendChild(el('span', 'msg-ver', verBits.join(' · ')));
    }
  }
  if (kind) {
    kind.textContent = MSG_KIND_TEXT[m.kind] || m.kind || '';
  }
  if (time) { time.textContent = fmtTime(m.createdAtMs); }

  if (body) {
    // 纯文本渲染：换行转成 <br>，不做任何 HTML 解析
    setPlainText(body, m.text || '');
  }

  if (foot) {
    clear(foot);
    var flags = [];
    if (m.streaming) { flags.push('流式输出中'); }
    if (m.unverified) { flags.push('未核实'); }
    if (m.operationState === 'unknown') { flags.push('操作结果未知'); }
    if (m.operationState === 'failed') { flags.push('操作失败'); }
    if (m.unknownCost) { flags.push('本次费用未知'); }
    for (var i = 0; i < flags.length; i++) {
      foot.appendChild(el('span', 'msg-flag', flags[i]));
    }
    show(foot, flags.length > 0);
  }

  box.appendChild(node);
  S.messages.push(m.id);
}

// 纯文本 + 换行。不用 innerHTML，也就不存在注入面。
function setPlainText(node, text) {
  clear(node);
  var lines = String(text).split('\n');
  for (var i = 0; i < lines.length; i++) {
    if (i > 0) { node.appendChild(document.createElement('br')); }
    node.appendChild(document.createTextNode(lines[i]));
  }
}

function scrollMessages() {
  var box = $('messages');
  if (!box) { return; }
  // 用户往上翻历史时不要抢滚动位置
  var nearBottom = (box.scrollHeight - box.scrollTop - box.clientHeight) < 120;
  if (nearBottom) { box.scrollTop = box.scrollHeight; }
}

/* ------------------------------------------------------------------ *
 * 成本面板
 * ------------------------------------------------------------------ */

function loadCost() {
  var dim = S.costDim;
  return getJSON('/api/cost?dim=' + encodeURIComponent(dim)).then(function (data) {
    S.cost = data;
    renderCost(data);
  }).catch(function (e) {
    var totals = $('cost-totals');
    if (totals) {
      clear(totals);
      totals.appendChild(el('div', 'cost-error', '成本读取失败：' + e.message));
    }
  });
}

function renderCost(data) {
  var totals = $('cost-totals');
  var unknown = $('cost-unknown');
  var body = $('cost-body');
  var limits = $('limits');
  if (!totals) { return; }

  clear(totals);

  // 未知金额：单独一条横幅，永远显示，绝不并进总额。
  // 未知必须有原因：后端给出 unknownReason 就原样展示（例如"桌面客户端不暴露
  // 每轮 token 用量"），没有才退回到通用说明。
  if (unknown) {
    if (data.unknownAmountCalls > 0) {
      var reason = data.unknownReason || '该通道不提供计费信息';
      unknown.textContent = '另有 ' + data.unknownAmountCalls +
        ' 次调用金额未知（' + reason + '），实际花费可能高于下方金额。';
      unknown.title = reason;
      show(unknown, true);
    } else {
      unknown.textContent = '';
      show(unknown, false);
    }
  }

  // 已知金额：分币种展示，不跨币种相加
  var money = fmtMoneyList(data.byCurrency);
  addCostRow(totals, '已知金额', money, 'amount');

  var callsText = fmtCount(data.calls) + ' 次调用';
  if (data.unknownAmountCalls > 0) {
    callsText += '（其中 ' + data.unknownAmountCalls + ' 次未知）';
  }
  addCostRow(totals, '调用', callsText, 'calls');

  var tokenText;
  if (data.callsWithTokens > 0) {
    tokenText = fmtCount(data.tokensIn) + ' 入 / ' + fmtCount(data.tokensOut) + ' 出';
    if (data.cacheReadTokens > 0) { tokenText += ' · 缓存读 ' + fmtCount(data.cacheReadTokens); }
    if (data.callsWithTokens < data.calls) {
      tokenText += '（' + (data.calls - data.callsWithTokens) + ' 次无 token 数据）';
    }
  } else {
    tokenText = UNKNOWN;
  }
  addCostRow(totals, 'tokens', tokenText, 'tokens');

  if (data.reservedCalls > 0) {
    addCostRow(totals, '已预留未结算', fmtCount(data.reservedCalls) + ' 次', 'reserved');
  }

  // 明细表
  if (body) {
    clear(body);
    var rows = data.rows || [];
    for (var i = 0; i < rows.length; i++) {
      body.appendChild(buildCostRow(rows[i]));
    }
    if (!rows.length) {
      var tr = el('tr');
      var td = el('td', 'cost-empty', '暂无数据');
      td.setAttribute('colspan', '4');
      tr.appendChild(td);
      body.appendChild(tr);
    }
  }

  // 客户端限额：桌面端不给硬上限，就写"未知"，不能假装有预算
  if (limits) {
    clear(limits);
    var ls = data.limits || [];
    for (var j = 0; j < ls.length; j++) {
      var li = el('li', 'limit-item');
      li.appendChild(el('span', 'limit-label', ls[j].label || ''));
      li.appendChild(el('span', 'limit-value', ls[j].limit || UNKNOWN));
      if (ls[j].note) { li.appendChild(el('span', 'limit-note', ls[j].note)); }
      limits.appendChild(li);
    }
    if (!ls.length) {
      limits.appendChild(el('li', 'limit-item', '本机未配置限额，也无法从客户端读取硬上限'));
    }
  }
}

function addCostRow(dl, label, value, key) {
  var dt = el('dt', 'cost-key', label);
  var dd = el('dd', 'cost-val', value);
  dd.setAttribute('data-key', key);
  dl.appendChild(dt);
  dl.appendChild(dd);
}

function buildCostRow(row) {
  var tr = el('tr');

  var tdKey = el('td', 'cost-row-key');
  tdKey.textContent = row.label || row.key || '';
  // 这一行有未知金额时，把原因写在行内，用户不用点开就知道为什么是未知
  if (row.unknownAmountCalls > 0) {
    var reason = row.unknownReason || row.note || '';
    if (reason) {
      tdKey.appendChild(el('span', 'cost-row-note', reason));
    }
  }
  tr.appendChild(tdKey);

  var tdCalls = el('td');
  var calls = fmtCount(row.calls);
  if (row.unknownAmountCalls > 0) { calls += '（' + row.unknownAmountCalls + ' 未知）'; }
  tdCalls.textContent = calls;
  tr.appendChild(tdCalls);

  var tdTok = el('td');
  tdTok.textContent = (row.callsWithTokens > 0)
    ? (fmtCount(row.tokensIn) + ' / ' + fmtCount(row.tokensOut))
    : UNKNOWN;
  tr.appendChild(tdTok);

  var tdMoney = el('td');
  tdMoney.textContent = fmtMoneyList(row.byCurrency);
  tr.appendChild(tdMoney);

  return tr;
}

function bindCostTabs() {
  var tabs = $('cost-tabs');
  if (!tabs) { return; }
  var btns = tabs.querySelectorAll('button');
  for (var i = 0; i < btns.length; i++) {
    btns[i].addEventListener('click', function (ev) {
      var btn = ev.currentTarget;
      var dim = btn.getAttribute('data-dim');
      if (!dim || dim === S.costDim) { return; }
      S.costDim = dim;
      for (var j = 0; j < btns.length; j++) {
        var on = btns[j] === btn;
        btns[j].className = on ? 'active' : '';
        btns[j].setAttribute('aria-selected', on ? 'true' : 'false');
      }
      loadCost();
    });
  }
}

/* ------------------------------------------------------------------ *
 * 输入框
 * ------------------------------------------------------------------ */

function bindComposer() {
  var form = $('composer');
  var input = $('input');
  if (!form || !input) { return; }

  form.addEventListener('submit', function (ev) {
    ev.preventDefault();
    sendMessage();
  });

  input.addEventListener('keydown', function (ev) {
    if (ev.key !== 'Enter') { return; }
    // Ctrl+Enter 发送；Shift+Ctrl+Enter 换行；单独 Enter 换行
    if (ev.ctrlKey && !ev.shiftKey) {
      ev.preventDefault();
      sendMessage();
    }
  });
}

function sendMessage() {
  var input = $('input');
  var btn = $('btn-send');
  var status = $('composer-status');
  if (!input) { return; }

  var text = input.value.trim();
  if (!text) {
    if (status) { status.textContent = '写点什么再发。'; }
    return;
  }

  var exSel = $('sel-executor');
  var rvSel = $('sel-reviewer');
  var payload = {
    text: text,
    // 客户端生成的幂等键：网络抖动重发不会产生两条消息
    clientMessageId: makeClientMessageId(),
    executor: exSel ? exSel.value : '',
    reviewer: rvSel ? rvSel.value : ''
  };

  if (btn) { btn.disabled = true; }
  if (status) { status.textContent = '发送中…'; }

  postJSON('/api/message', payload).then(function (res) {
    input.value = '';
    if (status) { status.textContent = ''; }
    if (res && res.message) { appendMessage(res.message); scrollMessages(); }
    scheduleSnapshot();
  }).catch(function (e) {
    // 失败时保留输入内容，别让用户白打字
    if (status) { status.textContent = '发送失败：' + e.message; }
  }).then(function () {
    if (btn) { btn.disabled = false; }
    input.focus();
  });
}

function makeClientMessageId() {
  // 不用 crypto.randomUUID 也行，但优先用它（本机 https/http 下可用）
  try {
    if (window.crypto && typeof window.crypto.randomUUID === 'function') {
      return window.crypto.randomUUID();
    }
  } catch (e) { /* 落到下面的兜底 */ }
  return 'cmid-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
}

/* ------------------------------------------------------------------ *
 * 控制按钮
 * ------------------------------------------------------------------ */

var CONTROL_LABEL = {
  pause: '暂停',
  resume: '继续',
  stop: '停止',
  replace: '换人'
};

function bindControls() {
  var box = $('decisions');
  if (!box) { return; }
  var btns = box.querySelectorAll('.decision-controls button');
  for (var i = 0; i < btns.length; i++) {
    btns[i].addEventListener('click', function (ev) {
      var action = ev.currentTarget.getAttribute('data-action');
      if (action) { sendControl(action); }
    });
  }
}

function sendControl(action) {
  if (!S.currentWorkId) {
    setControlMsg('当前没有进行中的委托。');
    return;
  }
  var label = CONTROL_LABEL[action] || action;

  if (action === 'stop') {
    // 停止是不可逆的破坏性操作，二次确认
    if (!window.confirm('确认停止当前委托？已经产生的结果会保留，但不会再有新的进展。')) {
      return;
    }
  }

  var body = { action: action };
  if (action === 'replace') {
    var exSel = $('sel-executor');
    if (!exSel || !exSel.value) {
      setControlMsg('换人需要先在下方选择新的执行 bot。');
      return;
    }
    body.executor = exSel.value;
  }

  setControlMsg(label + '中…');
  postJSON('/api/work/' + encodeURIComponent(S.currentWorkId) + '/control', body)
    .then(function (res) {
      // 后端可能回一句"停止待确认"——照抄它的话，别自己编"已停止"
      if (res && res.note) {
        setControlMsg(res.note);
      } else {
        setControlMsg('已提交：' + label);
      }
      scheduleSnapshot();
    })
    .catch(function (e) {
      setControlMsg(label + '失败：' + e.message);
    });
}

/* ------------------------------------------------------------------ *
 * SSE
 * ------------------------------------------------------------------ */

function connect() {
  closeStream();
  var url = '/api/events?cursor=' + encodeURIComponent(String(S.cursor));
  setConn('connecting', '连接中…');

  var es;
  try {
    es = new EventSource(url);
  } catch (e) {
    setConn('error', '无法建立实时连接');
    scheduleReconnect();
    return;
  }
  S.es = es;

  es.addEventListener('open', function () {
    S.reconnectDelay = RECONNECT_MIN_MS;
    setConn('live', '实时');
  });

  es.addEventListener('state', function (ev) {
    // 服务端主动推送的完整快照：省掉一次额外往返
    var data = parseFrame(ev.data);
    if (data) { applySnapshot(data); }
  });

  es.addEventListener('event', function (ev) {
    var e = parseFrame(ev.data);
    if (e) { onStreamEvent(e); }
  });

  es.addEventListener('ping', function () {
    // 心跳，只用于确认链路还活着
    if (S.es && S.es.readyState === 1) { setConn('live', '实时'); }
  });

  es.addEventListener('error', function () {
    // EventSource 自己会重连，但我们自己要能显示"断了"
    if (es.readyState === 2) {
      setConn('error', '连接已断开，正在重试…');
      scheduleReconnect();
    } else {
      setConn('connecting', '重连中…');
    }
  });
}

function closeStream() {
  if (S.es) {
    try { S.es.close(); } catch (e) { /* 忽略 */ }
    S.es = null;
  }
}

function scheduleReconnect() {
  closeStream();
  var delay = S.reconnectDelay;
  S.reconnectDelay = Math.min(RECONNECT_MAX_MS, Math.round(delay * 1.7));
  window.setTimeout(function () {
    if (!S.es) { connect(); }
  }, delay);
}

function parseFrame(raw) {
  if (!raw) { return null; }
  try { return JSON.parse(raw); } catch (e) { return null; }
}

function onStreamEvent(e) {
  // 游标推进：服务端 seq 单调，断线重连后从这里接着放
  if (typeof e.seq === 'number' && e.seq > S.cursor) { S.cursor = e.seq; }

  switch (e.kind) {
    case 'output':
      // 流式文本只用于显示，不触发唤醒，也不刷新快照
      applyOutputStream(e);
      break;

    case 'session':
      applySessionEvent(e);
      scheduleSnapshot();
      break;

    case 'progress':
      applyProgressEvent(e);
      break;

    case 'approval':
      // 有授权请求：立刻刷新，让"等你授权"置顶出现
      scheduleSnapshot();
      break;

    case 'final':
      applyFinalEvent(e);
      scheduleSnapshot();
      break;

    case 'error':
      applyErrorEvent(e);
      scheduleSnapshot();
      break;

    default:
      scheduleSnapshot();
  }
}

function applyOutputStream(e) {
  var box = $('messages');
  if (!box || !e.text) { return; }

  var msgId = e.botId ? ('stream-' + e.botId) : ('stream-' + (e.runId || 'x'));
  var node = box.querySelector('[data-id="' + cssEscapeAttr(msgId) + '"]');

  if (!node) {
    node = fromTemplate('tpl-msg');
    if (!node) { return; }
    node.setAttribute('data-id', msgId);
    node.setAttribute('data-author-kind', 'bot');
    node.setAttribute('data-kind', 'result');

    var author = node.querySelector('.msg-author');
    if (author) { author.textContent = (e.botName || e.botId || 'bot') + ' 输出中'; }
    var kind = node.querySelector('.msg-kind');
    if (kind) { kind.textContent = '流式'; }
    var body = node.querySelector('.msg-body');
    if (body) { body.textContent = ''; }
    box.appendChild(node);
  }

  var bodyNode = node.querySelector('.msg-body');
  if (bodyNode) {
    // 追加纯文本（流式片段不需要换行解析，避免半截 <br>）
    bodyNode.appendChild(document.createTextNode(String(e.text)));
  }
  S.streamingMsgId = msgId;
  scrollMessages();
}

function applySessionEvent(e) {
  var box = $('messages');
  if (!box) { return; }
  // 会话事件（新 Run 开始、会话切换）在流里留一条分隔，避免两条输出糊在一起
  var sep = el('div', 'msg-sep');
  sep.appendChild(el('span', null, e.text || '新一轮开始'));
  box.appendChild(sep);
  scrollMessages();
}

function applyProgressEvent(e) {
  var bots = S.snapshot && S.snapshot.bots;
  if (!bots) { return; }
  for (var i = 0; i < bots.length; i++) {
    if (bots[i].id !== e.botId) { continue; }
    if (e.text) { bots[i].statusDetail = e.text; }
    break;
  }
  renderBots(bots);
}

function applyFinalEvent(e) {
  var fin = e.final || {};
  // 收尾：把流式消息变成正式消息
  if (S.streamingMsgId) {
    var node = document.querySelector('[data-id="' + cssEscapeAttr(S.streamingMsgId) + '"]');
    if (node) {
      var author = node.querySelector('.msg-author');
      if (author) { author.textContent = (e.botName || e.botId || 'bot'); }
      var kind = node.querySelector('.msg-kind');
      if (kind) { kind.textContent = '结果'; }
      node.setAttribute('data-kind', 'result');
      var foot = node.querySelector('.msg-foot');
      if (foot) {
        clear(foot);
        if (fin.unverified) { foot.appendChild(el('span', 'msg-flag', '未核实')); }
        if (fin.success === false) { foot.appendChild(el('span', 'msg-flag', '失败')); }
        if (fin.usage && fin.usage.known === false) {
          foot.appendChild(el('span', 'msg-flag', '本次费用未知'));
        }
        show(foot, foot.childNodes.length > 0);
      }
    }
    S.streamingMsgId = '';
  }

  // 产物列表：挂在结果消息下面
  if (fin.artifacts && fin.artifacts.length) {
    var box = $('messages');
    if (box) {
      var wrap = el('div', 'msg-artifacts');
      wrap.appendChild(el('span', 'artifacts-label', '产物'));
      for (var i = 0; i < fin.artifacts.length; i++) {
        var a = fin.artifacts[i];
        var item = el('span', 'artifact');
        item.appendChild(el('span', 'artifact-kind', a.kind || ''));
        item.appendChild(el('span', 'artifact-ref', a.ref || ''));
        if (a.summary) { item.appendChild(el('span', 'artifact-summary', a.summary)); }
        wrap.appendChild(item);
      }
      box.appendChild(wrap);
      scrollMessages();
    }
  }
}

function applyErrorEvent(e) {
  var box = $('messages');
  if (!box) { return; }
  var node = el('div', 'msg-error');
  node.appendChild(el('span', 'msg-error-who', e.botName || e.botId || '系统'));
  node.appendChild(el('span', 'msg-error-text', e.text || '出错了'));
  box.appendChild(node);
  scrollMessages();
}

// 只用于拼选择器，不参与 HTML 解析
function cssEscapeAttr(v) {
  return String(v).replace(/["\\]/g, '\\$&');
}

/* ------------------------------------------------------------------ *
 * 启动
 * ------------------------------------------------------------------ */

function boot() {
  bindComposer();
  bindControls();
  bindCostTabs();

  loadSnapshot().then(function () {
    connect();
  }).catch(function () {
    // 快照失败也照样连流：流能通就还能用
    connect();
  });

  // 页面重新可见时补一次快照，防止后台标签页错过事件
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') {
      loadSnapshot();
      if (!S.es) { connect(); }
    }
  });

  // 关页面前收好连接
  window.addEventListener('beforeunload', closeStream);
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', boot);
} else {
  boot();
}
