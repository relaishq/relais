'use strict';

// Relais echo demo: planned handover.
//
// The demo runs two media workers, A and B, on one UDP socket; calls start on
// A. "Move call" (or window.relaisMove()) asks the server to hand the live
// call over to the other worker (POST /calls/{id}/move) and then measures,
// from this browser's side, what the move did to the call:
//
//   - connection, ICE and signaling state changes around the move (any
//     renegotiation or ICE restart shows up here);
//   - the longest interval between received echo video frames around the
//     move (requestVideoFrameCallback receiveTime; keep the tab visible);
//   - getStats deltas over the move window: audio and video packets received
//     and lost, concealed audio, frames decoded, dropped and frozen;
//   - the server's account (timings, state size, held packets) and the new
//     owner's count of this browser's packets it could not decrypt.
//
// Everything is published in window.relaisStats.handover (and #stats), next
// to how long the call has stayed connected since the last move and how many
// ICE consent checks were answered since then. Counters the browser does not
// expose read "unverified", never 0.

(() => {
  const SETTLE_MS = 3000; // how long after a move the page keeps measuring
  const LOOKBACK_MS = 1000; // frames before the move, for the typical interval
  const GAP_THRESHOLD_MS = 100; // issue #1's planned-handover threshold
  const MAX_FRAMES = 2000;
  const SAMPLE_INTERVAL_MS = 1000;
  const UNVERIFIED = 'unverified';

  const els = {
    move: document.getElementById('move'),
    remote: document.getElementById('remote'),
    summary: document.getElementById('handover'),
    moves: document.getElementById('moves'),
  };

  // tracked is the handover record of the current call's connection.
  let tracked = null;

  function currentCall() {
    return window.relaisCall ? window.relaisCall() : {};
  }

  function track(pc) {
    if (tracked && tracked.pc === pc) {
      return tracked;
    }
    const t = {
      pc,
      owner: 'A',
      moving: false,
      moves: [],
      events: [], // { at, kind, state }
      frames: [], // receive (or presentation) time of each echoed video frame
      latest: null, // the last getStats sample
    };
    tracked = t;
    const note = (kind, state) => {
      if (tracked === t) {
        t.events.push({ at: performance.now(), kind, state });
      }
    };
    pc.addEventListener('connectionstatechange', () => note('connection', pc.connectionState));
    pc.addEventListener('iceconnectionstatechange', () => note('ice', pc.iceConnectionState));
    pc.addEventListener('signalingstatechange', () => note('signaling', pc.signalingState));
    if ('requestVideoFrameCallback' in HTMLVideoElement.prototype) {
      const onFrame = (now, meta) => {
        if (tracked !== t) {
          return;
        }
        t.frames.push(typeof meta.receiveTime === 'number' ? meta.receiveTime : now);
        if (t.frames.length > MAX_FRAMES) {
          t.frames.splice(0, t.frames.length - MAX_FRAMES);
        }
        els.remote.requestVideoFrameCallback(onFrame);
      };
      els.remote.requestVideoFrameCallback(onFrame);
    }
    return t;
  }

  async function sample(pc) {
    const report = await pc.getStats();
    const all = [...report.values()];
    const inbound = (kind) => all.find((s) => s.type === 'inbound-rtp' && s.kind === kind) || {};
    const transport = all.find((s) => s.type === 'transport') || {};
    const pair = report.get(transport.selectedCandidatePairId)
      || all.find((s) => s.type === 'candidate-pair' && s.nominated && s.state === 'succeeded') || {};
    const audio = inbound('audio');
    const video = inbound('video');
    return {
      at: performance.now(),
      connectionState: pc.connectionState,
      iceConnectionState: pc.iceConnectionState,
      signalingState: pc.signalingState,
      dtlsState: transport.dtlsState,
      tlsVersion: transport.tlsVersion,
      selectedCandidatePairChanges: transport.selectedCandidatePairChanges,
      consentRequestsSent: pair.requestsSent,
      consentResponsesReceived: pair.responsesReceived,
      audio: {
        packetsReceived: audio.packetsReceived,
        packetsLost: audio.packetsLost,
        concealedSamples: audio.concealedSamples,
        concealmentEvents: audio.concealmentEvents,
      },
      video: {
        packetsReceived: video.packetsReceived,
        packetsLost: video.packetsLost,
        framesDecoded: video.framesDecoded,
        keyFramesDecoded: video.keyFramesDecoded,
        framesDropped: video.framesDropped,
        freezeCount: video.freezeCount,
        pliCount: video.pliCount,
      },
    };
  }

  // delta returns after - before for each counter, or "unverified".
  function delta(before, after) {
    const out = {};
    for (const key of Object.keys(after)) {
      const a = after[key];
      const b = before[key];
      out[key] = typeof a === 'number' && typeof b === 'number' ? a - b : UNVERIFIED;
    }
    return out;
  }

  function median(values) {
    if (values.length === 0) {
      return null;
    }
    const sorted = [...values].sort((x, y) => x - y);
    return sorted[Math.floor(sorted.length / 2)];
  }

  // videoGap is the longest interval between echoed frames from the last
  // frame before the move to the end of the window, and the typical (median)
  // interval in the second before the move.
  function videoGap(frames, start, end) {
    const before = frames.filter((f) => f >= start - LOOKBACK_MS && f <= start);
    const intervals = [];
    for (let i = 1; i < before.length; i += 1) {
      intervals.push(before[i] - before[i - 1]);
    }
    const last = before.length ? before[before.length - 1] : null;
    const inWindow = frames.filter((f) => last !== null && f >= last && f <= end);
    let gap = null;
    for (let i = 1; i < inWindow.length; i += 1) {
      gap = Math.max(gap || 0, inWindow[i] - inWindow[i - 1]);
    }
    return {
      maxFrameIntervalMs: gap === null ? UNVERIFIED : Math.round(gap),
      typicalFrameIntervalMs: intervals.length ? Math.round(median(intervals)) : UNVERIFIED,
      framesAfter: frames.filter((f) => f > start && f <= end).length,
    };
  }

  async function move() {
    const { pc, resourceURL, state } = currentCall();
    if (!pc || !resourceURL || state !== 'connected') {
      throw new Error('no connected call to move');
    }
    const t = track(pc);
    if (t.moving) {
      throw new Error('a move is already in progress');
    }
    t.moving = true;
    render();
    try {
      const before = await sample(pc);
      const start = performance.now();
      const response = await fetch(`${resourceURL}/move`, { method: 'POST' });
      const server = await response.json().catch(() => ({ ok: false, error: `HTTP ${response.status}` }));
      const returned = performance.now();
      await new Promise((resolve) => { setTimeout(resolve, SETTLE_MS); });
      if (tracked !== t) {
        throw new Error('the call ended during the move');
      }
      const after = await sample(pc);
      const end = performance.now();
      const status = await fetch(resourceURL).then((r) => (r.ok ? r.json() : null)).catch(() => null);

      const events = t.events.filter((e) => e.at >= start && e.at <= end)
        .map((e) => ({ atMs: Math.round(e.at - start), kind: e.kind, state: e.state }));
      const gap = videoGap(t.frames, start, end);
      const audio = delta(before.audio, after.audio);
      const video = delta(before.video, after.video);
      const browser = {
        connectionStateAfter: after.connectionState,
        iceConnectionStateAfter: after.iceConnectionState,
        dtlsStateAfter: after.dtlsState || UNVERIFIED,
        stateChangesDuringMove: events,
        selectedCandidatePairChanges: delta(before, after).selectedCandidatePairChanges,
        consentResponses: delta(before, after).consentResponsesReceived,
        video: { ...gap, ...video },
        audio: {
          ...audio,
          concealedMs: typeof audio.concealedSamples === 'number' ? Math.round(audio.concealedSamples / 48) : UNVERIFIED,
        },
      };
      if (server.ok) {
        t.owner = server.to;
      }
      const checks = {
        serverMoved: server.ok === true,
        stayedConnected: after.connectionState === 'connected'
          && events.every((e) => e.kind === 'signaling' || e.state === 'connected' || e.state === 'completed'),
        noRenegotiation: events.every((e) => e.kind !== 'signaling'),
        audioFlowing: typeof audio.packetsReceived === 'number' && audio.packetsReceived > 0,
        videoDecoding: typeof video.framesDecoded === 'number' && video.framesDecoded > 0,
        videoGapUnder100ms: typeof gap.maxFrameIntervalMs === 'number' ? gap.maxFrameIntervalMs < GAP_THRESHOLD_MS : UNVERIFIED,
        ownerDecryptFailures: status ? status.ownerDecryptFailures : UNVERIFIED,
      };
      const entry = {
        n: t.moves.length + 1,
        at: new Date().toISOString(),
        from: server.from,
        to: server.to,
        requestMs: Math.round(returned - start),
        windowMs: Math.round(end - start),
        server,
        browser,
        checks,
        pass: Object.entries(checks).every(([key, value]) => (key === 'ownerDecryptFailures' ? value === 0 : value === true)),
        // The server's moves are counted from the start of the call; the
        // browser's from this connection.
        startedAt: start,
        consentBefore: before.consentResponsesReceived,
      };
      t.moves.push(entry);
      log(`move ${entry.n} ${entry.from} -> ${entry.to}: ${entry.pass ? 'PASS' : 'FAIL'}; server ${server.durationMs} ms, `
        + `video max frame interval ${gap.maxFrameIntervalMs} ms (typical ${gap.typicalFrameIntervalMs}), `
        + `audio concealed ${browser.audio.concealedMs} ms, lost audio/video ${audio.packetsLost}/${video.packetsLost}`);
      return entry;
    } finally {
      t.moving = false;
      render();
    }
  }

  function log(message) {
    const el = document.getElementById('log');
    if (el) {
      el.textContent += `${new Date().toISOString().slice(11, 23)}  ${message}\n`;
    }
    console.log(`[relais] ${message}`);
  }

  // stats is the handover part of window.relaisStats. After hangup it keeps
  // describing the call that ended, until the next call connects.
  function stats() {
    const t = tracked;
    const { pc } = currentCall();
    if (!t || (pc && pc !== t.pc)) {
      return { owner: null, moves: [] };
    }
    const live = pc === t.pc;
    if (!live && t.endedAt === undefined) {
      t.endedAt = performance.now();
    }
    const last = t.moves[t.moves.length - 1];
    const now = live ? performance.now() : t.endedAt;
    const latest = t.latest;
    let sinceLastMove = null;
    if (last) {
      const broken = t.events.some((e) => e.at >= last.startedAt && e.kind !== 'signaling'
        && e.state !== 'connected' && e.state !== 'completed');
      sinceLastMove = {
        ms: Math.round(now - last.startedAt),
        callEnded: !live,
        stayedConnected: !broken && (!live || t.pc.connectionState === 'connected'),
        consentResponses: latest && typeof latest.consentResponsesReceived === 'number'
          && typeof last.consentBefore === 'number'
          ? latest.consentResponsesReceived - last.consentBefore : UNVERIFIED,
      };
    }
    return {
      owner: t.owner,
      moving: t.moving,
      moveCount: t.moves.length,
      allPassed: t.moves.length > 0 && t.moves.every((m) => m.pass),
      sinceLastMove,
      consent: latest ? {
        requestsSent: latest.consentRequestsSent === undefined ? UNVERIFIED : latest.consentRequestsSent,
        responsesReceived: latest.consentResponsesReceived === undefined ? UNVERIFIED : latest.consentResponsesReceived,
      } : null,
      stateChanges: t.events.map((e) => ({ atMs: Math.round(e.at), kind: e.kind, state: e.state })),
      moves: t.moves.map(({ startedAt, consentBefore, ...rest }) => rest),
    };
  }

  function render() {
    const s = stats();
    const { state } = currentCall();
    els.move.disabled = state !== 'connected' || s.moving === true;
    const last = s.moves[s.moves.length - 1];
    const rows = [
      ['Owner', s.owner ? `worker ${s.owner}` : '-'],
      ['Moves', s.moveCount ? `${s.moveCount} (${s.allPassed ? 'all passed' : 'some failed'})` : '0'],
      ['Last move', last
        ? `${last.from} → ${last.to}: ${last.pass ? 'PASS' : 'FAIL'}, video max frame interval `
          + `${last.browser.video.maxFrameIntervalMs} ms (typical ${last.browser.video.typicalFrameIntervalMs} ms)`
        : '-'],
      ['Since last move', s.sinceLastMove
        ? `${Math.round(s.sinceLastMove.ms / 1000)} s, connected throughout: ${s.sinceLastMove.stayedConnected}, `
          + `consent checks answered: ${s.sinceLastMove.consentResponses}`
        : '-'],
    ];
    els.summary.replaceChildren(...rows.flatMap(([name, value]) => {
      const dt = document.createElement('dt');
      dt.textContent = name;
      const dd = document.createElement('dd');
      dd.textContent = String(value);
      return [dt, dd];
    }));
    els.moves.textContent = JSON.stringify(s.moves, null, 2);
  }

  async function poll() {
    const { pc, state } = currentCall();
    if (pc && state === 'connected') {
      const t = track(pc);
      try {
        t.latest = await sample(pc);
      } catch (err) {
        // the call ended while sampling
      }
    }
    render();
  }

  window.relaisHandover = { stats };
  window.relaisMove = move;
  els.move.addEventListener('click', () => {
    move().catch((err) => log(`move: ${err.message || err}`));
  });
  setInterval(poll, SAMPLE_INTERVAL_MS);
  render();
})();
