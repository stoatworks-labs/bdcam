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
    var chosen = String(current.outputs || '').split(',').map(function (s) { return s.trim(); });
    function box(id, val, label, hint) {
      return '<label style="display:block;margin:0 0 2px 0;font-weight:normal;">' +
        '<input type="checkbox" id="' + id + '" value="' + val + '"' +
        (chosen.indexOf(val) !== -1 ? ' checked' : '') + ' style="width:auto;margin-right:6px;vertical-align:middle;">' +
        label + ' <span style="opacity:.65;font-size:11px;">' + hint + '</span></label>';
    }

    var formats = (caps && caps.formats) || ['auto'];
    var size = current.width + 'x' + current.height;
    if (SIZES.indexOf(size) === -1) SIZES.unshift(size);

    root.innerHTML =
      '<div class="row m-0 pt-2 div_tab_contents">' +
        '<div class="col-xl-6 m-0 p-0 pr-xl-2"><div>' +
          row('Converter', '<select id="bdc_enabled">' +
              '<option value="0"' + (!current.enabled ? ' selected' : '') + '>Off</option>' +
              '<option value="1"' + (current.enabled ? ' selected' : '') + '>On</option></select>') +
          row('Output',
              box('bdc_out_ndi', 'ndi', 'NDI', '— uncompressed, on the CPU') +
              box('bdc_out_srt', 'srt', 'SRT', '— H.264 from the hardware encoder') +
              box('bdc_out_hdmi', 'hdmi', 'HDMI', '— straight to the panel') +
              '<div id="bdc_out_hint" style="font-size:11px;opacity:.7;margin-top:3px;"></div>') +
          row('HDMI via', '<select id="bdc_hdmimode">' +
              '<option value="direct"' + (current.hdmi_mode !== 'decoder' ? ' selected' : '') + '>Direct — takes the display from the decoder</option>' +
              '<option value="decoder"' + (current.hdmi_mode === 'decoder' ? ' selected' : '') + '>Via the decoder — not working on this firmware</option>' +
              '</select>', 'bdc_row_hdmimode') +
          row('Camera',
              '<div style="display:flex;gap:6px;align-items:center;">' +
                '<select id="bdc_device" style="flex:1;min-width:0;"></select>' +
                '<button type="button" id="bdc_detect" class="restart" style="white-space:nowrap;padding:4px 10px;">DETECT</button>' +
              '</div>') +
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

    OUTPUT_BOXES.forEach(function (b) {
      var e = document.getElementById(b[0]);
      if (e) e.addEventListener('change', onOutputsChange);
    });
    document.getElementById('bdc_save').addEventListener('click', save);
    var hm = document.getElementById('bdc_hdmimode');
    if (hm) hm.addEventListener('change', onOutputsChange);
    document.getElementById('bdc_detect').addEventListener('click', detect);
    onOutputsChange();
    fillDevices();
    if (caps && caps.notes) {
      document.getElementById('bdc_notes').innerHTML =
        '<b>Notes for this hardware</b><ul style="padding-left:1.2em;margin:.3em 0;">' +
        caps.notes.map(function (n) { return '<li>' + esc(n) + '</li>'; }).join('') + '</ul>';
    }
  }

  // SRT and HDMI share one capture through a tee, so they combine freely. NDI
  // cannot join them: it captures V4L2 itself while GStreamer owns the camera
  // for the other two, and one device cannot have two owners.
  var OUTPUT_BOXES = [
    ['bdc_out_ndi', 'ndi'],
    ['bdc_out_srt', 'srt'],
    ['bdc_out_hdmi', 'hdmi']
  ];

  function selectedOutputs() {
    return OUTPUT_BOXES.filter(function (b) {
      var e = document.getElementById(b[0]);
      return e && e.checked;
    }).map(function (b) { return b[1]; }).join(',');
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  // Only show the fields that the chosen outputs actually use, and warn about
  // the things that will otherwise surprise people.
  function onOutputsChange() {
    var v = selectedOutputs();
    show('bdc_row_ndi', v.indexOf('ndi') !== -1);
    show('bdc_row_srt', v.indexOf('srt') !== -1);
    show('bdc_row_hdmimode', v.indexOf('hdmi') !== -1);

    var hint = document.getElementById('bdc_out_hint');
    if (hint) {
      hint.textContent = v === ''
        ? 'Pick at least one output.'
        : 'All three share one capture, so they can run together.';
    }

    var warn = '';
    var mode = (document.getElementById('bdc_hdmimode') || {}).value;
    if (v.indexOf('hdmi') !== -1) {
      if (mode === 'decoder') {
        warn = 'The decoder route renders green on this firmware — PPApp does not display our stream correctly. Use Direct.';
      } else {
        // Both costs are real and measured; people should know before they
        // turn it on and wonder why everything slowed down.
        warn = 'Switching HDMI on lowers the frame rate of every output — the display needs another conversion per frame on the CPU. ' +
               'Measured at about 11 fps at 1080p with NDI alongside, against about 18 fps for NDI on its own. ' +
               'Direct HDMI also takes the display from the decoder: the OSD, web UI video and tally stop until the converter is switched off.';
      }
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

  // Rescanning keeps whatever camera was already chosen, so pressing DETECT to
  // check for a newly plugged device does not silently change the selection.
  function fillDevices() {
    var sel = document.getElementById('bdc_device');
    if (!sel) return Promise.resolve(0);
    var wanted = sel.value || current.device || '';
    return fetch(api + '/api/devices').then(r => r.json()).then(function (d) {
      var devices = d.devices || [];
      sel.innerHTML = devices.length ? '<option value="">Auto-detect</option>'
                                     : '<option value="">No camera detected</option>';
      devices.forEach(function (dev) {
        var o = document.createElement('option');
        o.value = dev.path;
        o.textContent = dev.path + (dev.formats && dev.formats.length ? ' (' + dev.formats.join(', ') + ')' : '');
        if (wanted === dev.path) o.selected = true;
        sel.appendChild(o);
      });
      return devices.length;
    }).catch(function () { return -1; });
  }

  // The camera on this device is hot-pluggable and the streamer only looks for
  // one when it starts, so being able to rescan without a reload is worth a
  // button.
  function detect() {
    var b = document.getElementById('bdc_detect');
    var label = b.textContent;
    b.disabled = true;
    b.textContent = '…';
    setMsg('Scanning for cameras…', '');
    fillDevices().then(function (n) {
      b.disabled = false;
      b.textContent = label;
      if (n < 0) {
        setMsg('Could not reach the converter service on port ' + API_PORT, 'error');
      } else if (n === 0) {
        setMsg('No camera detected. Check it is plugged into the USB-A port and powered — a bus-powered camera behind an unpowered hub often will not enumerate.', 'warn');
      } else {
        setMsg(n + (n === 1 ? ' camera' : ' cameras') + ' detected.', '');
      }
      refreshStatus();
    });
  }

  function setMsg(text, kind) {
    var m = document.getElementById('bdc_msg');
    if (!m) return;
    m.textContent = text || '';
    m.style.color = kind === 'error' ? '#c0392b' : (kind === 'warn' ? '#b8860b' : '');
  }

  function save() {
    if (selectedOutputs() === '') {
      setMsg('Pick at least one output before applying.', 'error');
      return;
    }
    var size = document.getElementById('bdc_size').value.split('x');
    var body = {
      enabled: document.getElementById('bdc_enabled').value === '1',
      outputs: selectedOutputs(),
      device: document.getElementById('bdc_device').value,
      width: parseInt(size[0], 10),
      height: parseInt(size[1], 10),
      fps: parseInt(document.getElementById('bdc_fps').value, 10),
      format: document.getElementById('bdc_format').value,
      ndi_name: document.getElementById('bdc_ndiname').value,
      srt_url: document.getElementById('bdc_srturl').value,
      synthetic: document.getElementById('bdc_synthetic').value === '1',
      hdmi_mode: (document.getElementById('bdc_hdmimode') || {}).value || 'decoder',
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
