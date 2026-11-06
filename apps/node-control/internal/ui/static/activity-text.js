// headlamp console: the sentence an activity entry reads as. Shared by the Overview and Activity pages, so both say
// the same. Needs hl.js first. The input is plain text from the network; the output is text for textContent.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  function head(s, sep) {
    var i = s.indexOf(sep);
    return i < 0 ? s : s.slice(0, i);
  }

  // describe turns an entry's action and detail into the sentence the table shows. The modules write fixed details
  // (names, ids, dates), so the sentence is built from them; anything not recognised is shown as it is.
  function describe(e) {
    var action = hl.plain(e && e.action, 100);
    var detail = hl.plain(e && e.detail, 500);
    var m;
    switch (action) {
      case 'device.create':
        return head(detail, ', expires ') + ' enrolled';
      case 'device.link':
        return 'Enrolment link made for ' + detail;
      case 'device.revoke':
        return detail + ' revoked';
      case 'device.delete':
        return detail + ' deleted';
      case 'device.extend':
        m = /^(.*), expires (\S+)$/.exec(detail);
        return m ? m[1] + ' extended until ' + m[2] : detail + ' extended';
      case 'device.rotate_secret':
        return 'Secret of ' + detail + ' rotated';
      case 'bridge.approve':
        return 'Bridge ' + detail + ' approved';
      case 'bridge.unapprove':
        return 'Approval of bridge ' + detail + ' withdrawn';
      case 'node.start':
      case 'node.stop':
        m = /^(.*): (ok|failed \(([^)]*)\))$/.exec(detail);
        if (m) {
          var stopped = action === 'node.stop';
          if (m[2] === 'ok') { return m[1] + (stopped ? ' stopped' : ' started'); }
          return (stopped ? 'Stopping ' : 'Starting ') + m[1] + ' failed (' + m[3] + ')';
        }
        break;
      case 'namespace.create':
        m = /^(\S+) ports (.+)$/.exec(detail);
        return m ? 'Namespace ' + m[1] + ' created, ports ' + m[2] : 'Namespace ' + detail + ' created';
      case 'namespace.ports':
        m = /^(\S+) ports (.+)$/.exec(detail);
        return m ? 'Namespace ' + m[1] + ' ports set to ' + m[2] : 'Namespace ' + detail + ' ports changed';
      case 'namespace.delete':
        return 'Namespace ' + detail + ' deleted';
      case 'key.mint':
        m = /^(.*) id (\d+)\b/.exec(detail);
        return m ? 'Key ' + m[2] + ' minted for ' + m[1] : 'Key minted';
      case 'key.expire':
        m = /^id (\d+)$/.exec(detail);
        return m ? 'Key ' + m[1] + ' expired' : 'Key expired';
      case 'route.put':
        m = /^([^/\s]+)\/(\S+) port (\d+)$/.exec(detail);
        return m ? 'Route ' + m[2] + '.' + m[1] + ' set, port ' + m[3] : 'Route set';
      case 'route.delete':
        m = /^([^/\s]+)\/(\S+)$/.exec(detail);
        return m ? 'Route ' + m[2] + '.' + m[1] + ' removed' : 'Route removed';
      case 'block.put':
        m = /^([^/\s]+)\/(\S+)$/.exec(detail);
        return m ? m[2] + '.' + m[1] + ' blocked' : 'Service blocked';
      case 'block.delete':
        m = /^([^/\s]+)\/(\S+)$/.exec(detail);
        return m ? m[2] + '.' + m[1] + ' unblocked' : 'Service unblocked';
      case 'attention.acknowledge':
        return 'Acknowledged: ' + detail;
      case 'attention.unacknowledge':
        return 'Acknowledgement undone: ' + detail;
      case 'audit.unreadable':
        return 'An audit entry could not be read';
      case 'change.applied':
        m = /^(\d+)(?:: (.*))?$/.exec(detail);
        if (m) { return 'Change #' + m[1] + ' applied' + (m[2] ? ': ' + m[2] : ''); }
        break;
      case 'change.confirmed':
        m = /^(\d+)$/.exec(detail);
        if (m) { return 'Change #' + m[1] + ' confirmed'; }
        break;
      case 'change.reverted_earlier':
        m = /^(\d+)$/.exec(detail);
        if (m) { return 'Change #' + m[1] + ' reverted'; }
        break;
      case 'change.reverted':
        m = /^(\d+)$/.exec(detail);
        if (m) { return 'Change #' + m[1] + ' reverted by itself after no confirmation'; }
        break;
      case 'change.reverted_on_request':
        m = /^(\d+)$/.exec(detail);
        if (m) { return 'Change #' + m[1] + ' reverted on request'; }
        break;
      case 'task.manual':
      case 'task.run':
        m = /^(.*): (succeeded|failed|lost|cancelled)$/.exec(detail);
        if (m) {
          if (action === 'task.manual') { return m[1] + ' run by hand' + (m[2] === 'succeeded' ? '' : ', ' + m[2]); }
          return m[1] + ' run ' + m[2];
        }
        break;
    }
    return action ? action + (detail ? ': ' + detail : '') : detail;
  }

  hl.activityText = { describe: describe };
})();
