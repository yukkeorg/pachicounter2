// PachiCounter のフロント。
//
// やることはコアが流すスナップショットを属性とテキストに流し込むことだけで、
// 機種ごとの判断は一切しない。機種固有の数字は metrics にラベル付きで来るので、
// 知らない機種でもそのまま並べて表示できる。
// 詳細は docs/adr/0002-plugin-owns-domain-front-owns-look.md を参照。

'use strict';

const el = (id) => document.getElementById(id);

const nodes = {
  machine: el('machine'),
  state: el('state'),
  currentRotations: el('current-rotations'),
  totalRotations: el('total-rotations'),
  firstHitRate: el('first-hit-rate'),
  bonuses: el('bonuses'),
  firstHits: el('first-hits'),
  chainField: el('chain-field'),
  chain: el('chain'),
  ballsLabel: el('balls-label'),
  ballsHeld: el('balls-held'),
  ballsNote: el('balls-note'),
  metrics: el('metrics'),
  normalRotations: el('normal-rotations'),
  secPerRotation: el('sec-per-rotation'),
  device: el('device'),
};

/** 数値を桁区切りで整形する。 */
function num(value, digits = 0) {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return '—';
  }
  return value.toLocaleString('ja-JP', {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

/** 初当たり確率を 1/xxx.x の形にする。まだ当たっていなければ伏せる。 */
function rate(value) {
  if (!value || value <= 0) {
    return '1/-';
  }
  return `1/${value.toFixed(1)}`;
}

function renderMetrics(metrics) {
  nodes.metrics.replaceChildren();
  if (!Array.isArray(metrics)) {
    return;
  }

  for (const metric of metrics) {
    const item = document.createElement('li');

    const label = document.createElement('span');
    label.className = 'metrics__label';
    label.textContent = metric.label || metric.key;

    const value = document.createElement('span');
    value.className = 'metrics__value';
    value.textContent = metric.unit === '1/x'
      ? rate(metric.value)
      : num(metric.value, metric.digits || 0) + (metric.unit ? ` ${metric.unit}` : '');

    item.append(label, value);
    nodes.metrics.append(item);
  }
}

function render(snap) {
  const counters = snap.counters || {};
  const derived = snap.derived || {};
  const state = snap.state || {};
  const device = snap.device || {};

  document.body.dataset.state = state.kind || 'normal';
  document.body.dataset.machine = (snap.machine && snap.machine.id) || '';
  document.body.dataset.connected = device.connected ? 'true' : 'false';

  nodes.machine.textContent = (snap.machine && snap.machine.name) || '—';
  nodes.state.textContent = state.label || '';

  nodes.currentRotations.textContent = num(counters.current_rotations);
  nodes.totalRotations.textContent = `/ ${num(derived.total_rotations)}`;
  nodes.firstHitRate.textContent = rate(derived.first_hit_rate);
  nodes.bonuses.textContent = num(counters.bonuses);
  nodes.firstHits.textContent = `(${num(counters.first_hits)})`;

  const chain = counters.chain || 0;
  nodes.chainField.hidden = chain <= 0;
  nodes.chain.textContent = num(chain);

  nodes.ballsHeld.textContent = num(Math.round(derived.balls_held));
  nodes.ballsLabel.textContent = '持玉';
  // 獲得玉数が実測か推定かは数字の意味が変わるので、隠さずに出す。
  nodes.ballsNote.textContent = derived.balls_gained_measured ? '(実測)' : '(推定)';

  nodes.normalRotations.textContent = `通常 ${num(counters.normal_rotations)} / 電サポ ${num(counters.densapo_rotations)}`;
  nodes.secPerRotation.textContent = derived.sec_per_rotation
    ? `${derived.sec_per_rotation.toFixed(2)} 秒/回転`
    : '—';
  nodes.device.textContent = device.connected
    ? device.source || '接続中'
    : `${device.source || '信号源'} を待っています`;

  renderMetrics(snap.metrics);
}

function connect() {
  // EventSource はブラウザが自動で再接続する。配信中にコアを再起動しても、
  // フロント側に再接続の実装を持たずに復帰できる。
  const events = new EventSource('/events');

  events.addEventListener('snapshot', (message) => {
    try {
      render(JSON.parse(message.data));
    } catch (err) {
      console.error('スナップショットを解釈できませんでした', err);
    }
  });

  events.addEventListener('error', () => {
    document.body.dataset.connected = 'false';
    nodes.device.textContent = 'コアに接続できません';
  });
}

connect();
