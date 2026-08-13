/* UVC Converter tab for the BirdDog PLAY web UI.
 *
 * Served from /static/, which is outside the Go template system, so the patch
 * to videoset.html stays three lines and this file carries all the behaviour.
 *
 * Talks to bdcam's configuration API, which runs as its own process on
 * BDCAM_API_PORT. It is deliberately a separate service from the streamer: the
 * streamer exits when no camera is attached, and a settings page that vanished
 * with the camera would be useless.
 */
(function () {
  'use strict';

  var API_PORT = 8090;
  var api = location.protocol + '//' + location.hostname + ':' + API_PORT;
  var root, caps, current, statusTimer;

  var SIZES = ['1920x1080', '1280x720', '960x540', '640x360'];
  var RATES = [60, 50, 30, 25, 24, 15];

  function el(html) {
    var d = document.createElement('div');
    d.innerHTML = html.trim();
    return d.firstChild;
  }

  function row(label, controlHTML, id) {
    return '<div class="row m-0 p-1"' + (id ? ' id="' + id + '"' : '') + '>' +
      '<div class="col-xl-5 m-0 p-0 my-auto">' + label + '</div>' +
      '<div class="col-xl-7 m-0 p-0">' + controlHTML + '</div>' +
      '</div>';
  }

  function options(values, selected) {
    return values.map(function (v) {
      return '<option value="' + v + '"' + (String(v) === String(selected) ? ' selected' : '') + '>' + v + '</option>';
    }).join('');
  }

  function render() {
    var outputs = [
      ['ndi', 'NDI (uncompressed)'],
      ['srt', 'SRT (H.264)'],
      ['hdmi', 'HDMI out'],
      ['srt,hdmi', 'SRT + HDMI']
    ];
    var outOpts = outputs.map(function (o) {
      return '<option value="' + o[0] + '"' + (current.outputs === o[0] ? ' selected' : '') + '>' + o[1] + '</option>';
    }).join('');

    var formats = (caps && caps.formats) || ['auto'];
    var size = current.width + 'x' + current.height;
    if (SIZES.indexOf(size) === -1) SIZES.unshift(size);

    root.innerHTML =
      '<div class="row m-0 pt-2 div_tab_contents">' +
        '<div class="col-xl-6 m-0 p-0 pr-xl-2"><div>' +
          row('Converter', '<select id="bdc_enabled">' +
              '<option value="0"' + (!current.enabled ? ' selected' : '') + '>Off</option>' +
              '<option value="1"' + (current.enabled ? ' selected' : '') + '>On</option></select>') +
          row('Output', '<select id="bdc_outputs">' + outOpts + '</select>') +
          row('Camera', '<select id="bdc_device"></select>') +
          row('Pixel format', '<select id="bdc_format">' + options(formats, current.format) + '</select>') +
          row('Resolution', '<select id="bdc_size">' + options(SIZES, size) + '</select>') +
          row('Frame rate', '<select id="bdc_fps">' + options(RATES, current.fps) + '</select>') +
          row('Test pattern', '<select id="bdc_synthetic">' +
              '<option value="0"' + (!current.synthetic ? ' selected' : '') + '>Off</option>' +
              '<option value="1"' + (current.synthetic ? ' selected' : '') + '>On (no camera needed)</option></select>') +
          row('NDI source name', '<input type="text" id="bdc_ndiname" value="' + esc(current.ndi_name) + '" placeholder="&lt;hostname&gt; (Cam)" />', 'bdc_row_ndi') +
          row('SRT destination', '<input type="text" id="bdc_srturl" value="' + esc(current.srt_url) + '" placeholder="srt://host:9000?streamid=cam" />', 'bdc_row_srt') +
          '<div class="row m-0 p-1"><div class="col-xl-5 m-0 p-0"></div>' +
            '<div class="col-xl-7 m-0 p-0"><button class="restart" type="button" id="bdc_save">APPLY</button></div>' +
          '</div>' +
          '<div class="row m-0 p-1"><div class="col-xl-12 m-0 p-0"><div id="bdc_msg"></div></div></div>' +
        '</div></div>' +
        '<div class="col-xl-6 m-0 p-0 pl-xl-2"><div>' +
          '<div class="row m-0 p-1"><div class="col-xl-12 m-0 p-0"><b>Status</b></div></div>' +
          '<div class="row m-0 p-1"><div class="col-xl-12 m-0 p-0"><div id="bdc_status">checking…</div></div></div>' +
          '<div class="row m-0 p-1"><div class="col-xl-12 m-0 p-0"><b>Recent log</b></div></div>' +
          '<div class="row m-0 p-1"><div class="col-xl-12 m-0 p-0">' +
            '<pre id="bdc_log" style="max-height:16em;overflow:auto;font-size:11px;white-space:pre-wrap;"></pre>' +
          '</div></div>' +
          '<div class="row m-0 p-1"><div class="col-xl-12 m-0 p-0"><div id="bdc_notes" style="font-size:11px;opacity:.8;"></div></div></div>' +
        '</div></div>' +
      '</div>';

    document.getElementById('bdc_outputs').addEventListener('change', onOutputsChange);
    document.getElementById('bdc_save').addEventListener('click', save);
    onOutputsChange();
    fillDevices();
    if (caps && caps.notes) {
      document.getElementById('bdc_notes').innerHTML =
        '<b>Notes for this hardware</b><ul style="padding-left:1.2em;margin:.3em 0;">' +
        caps.notes.map(function (n) { return '<li>' + esc(n) + '</li>'; }).join('') + '</ul>';
    }
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  // Only show the fields that the chosen output actually uses, and warn about
  // the two things that will otherwise surprise people.
  function onOutputsChange() {
    var v = document.getElementById('bdc_outputs').value;
    show('bdc_row_ndi', v === 'ndi');
    show('bdc_row_srt', v.indexOf('srt') !== -1);
    var warn = '';
    if (v.indexOf('hdmi') !== -1) {
      warn = 'HDMI output takes over the display from the decoder. The normal ' +
             'PLAY output stops until the converter is switched off again.';
    }
    if (caps && !caps.bitrate_adjustable && v.indexOf('srt') !== -1) {
      warn += (warn ? ' ' : '') +
        'The hardware encoder on this firmware has no bitrate setting; it is derived from resolution and frame rate.';
    }
    setMsg(warn, warn ? 'warn' : '');
  }

  function show(id, on) {
    var e = document.getElementById(id);
    if (e) e.style.display = on ? '' : 'none';
  }

  function fillDevices() {
    var sel = document.getElementById('bdc_device');
    if (!sel) return;
    sel.innerHTML = '<option value="">Auto-detect</option>';
    fetch(api + '/api/devices').then(r => r.json()).then(function (d) {
      (d.devices || []).forEach(function (dev) {
        var o = document.createElement('option');
        o.value = dev.path;
        o.textContent = dev.path + (dev.formats ? ' (' + dev.formats.join(', ') + ')' : '');
        if (current.device === dev.path) o.selected = true;
        sel.appendChild(o);
      });
      if (!(d.devices || []).length) {
        sel.innerHTML = '<option value="">No camera detected</option>';
      }
    }).catch(function () { /* status panel reports the outage */ });
  }

  function setMsg(text, kind) {
    var m = document.getElementById('bdc_msg');
    if (!m) return;
    m.textContent = text || '';
    m.style.color = kind === 'error' ? '#c0392b' : (kind === 'warn' ? '#b8860b' : '');
  }

  function save() {
    var size = document.getElementById('bdc_size').value.split('x');
    var body = {
      enabled: document.getElementById('bdc_enabled').value === '1',
      outputs: document.getElementById('bdc_outputs').value,
      device: document.getElementById('bdc_device').value,
      width: parseInt(size[0], 10),
      height: parseInt(size[1], 10),
      fps: parseInt(document.getElementById('bdc_fps').value, 10),
      format: document.getElementById('bdc_format').value,
      ndi_name: document.getElementById('bdc_ndiname').value,
      srt_url: document.getElementById('bdc_srturl').value,
      synthetic: document.getElementById('bdc_synthetic').value === '1',
      connector: current.connector || 0
    };
    setMsg('Applying…', '');
    fetch(api + '/api/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (r) {
      return r.json().then(function (j) { return { ok: r.ok, body: j }; });
    }).then(function (res) {
      if (!res.ok) {
        setMsg(res.body.error || 'Could not apply settings', 'error');
        return;
      }
      current = res.body.config;
      setMsg(res.body.message || 'Applied', '');
      refreshStatus();
    }).catch(function (e) {
      setMsg('Could not reach the converter service: ' + e, 'error');
    });
  }

  function refreshStatus() {
    fetch(api + '/api/status').then(r => r.json()).then(function (s) {
      var bits = [
        'Service: <b>' + esc(s.service_state) + '</b>',
        'Camera: <b>' + (s.camera_found ? esc((s.devices || []).join(', ')) : 'none detected') + '</b>'
      ];
      if (s.drm_held_by) bits.push('Display held by: <b>' + esc(s.drm_held_by) + '</b>');
      document.getElementById('bdc_status').innerHTML = bits.join('<br>');
      document.getElementById('bdc_log').textContent = (s.last_log || []).join('\n');
    }).catch(function () {
      var e = document.getElementById('bdc_status');
      if (e) e.innerHTML = '<span style="color:#c0392b">Converter service not reachable on port ' + API_PORT + '</span>';
    });
  }

  function init() {
    root = document.getElementById('uvc_form');
    if (!root) return;
    root.innerHTML = '<div class="row m-0 p-3">Loading converter settings…</div>';

    Promise.all([
      fetch(api + '/api/capabilities').then(r => r.json()),
      fetch(api + '/api/config').then(r => r.json())
    ]).then(function (r) {
      caps = r[0];
      current = r[1];
      render();
      refreshStatus();
      if (statusTimer) clearInterval(statusTimer);
      statusTimer = setInterval(function () {
        if (root.style.display !== 'none') refreshStatus();
      }, 5000);
    }).catch(function (e) {
      root.innerHTML = '<div class="row m-0 p-3" style="color:#c0392b">' +
        'The converter service is not answering on port ' + API_PORT + '.<br>' +
        'Check it with: <code>systemctl status bd-cam-api</code></div>';
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
