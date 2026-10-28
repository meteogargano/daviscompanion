'use strict';
'require view';
'require form';
'require rpc';

var callInitList = rpc.declare({
	object: 'luci',
	method: 'getInitList',
	params: [ 'name' ]
});

return view.extend({
	title: _('Davis Weather'),

	load: function() {
		return callInitList('davisweather').catch(function() { return {}; });
	},

	render: function(initList) {
		var m, s, o;

		var state = (initList && initList.davisweather) || {};
		var statusText = state.running
			? _('Service is running.')
			: _('Service is not running (check "Enabled" and logread).');

		m = new form.Map('davisweather', _('Davis Weather Companion'),
			_('Reads a Davis Vantage Pro / Pro2 / Vue console over the WeatherLink serial interface and uploads measurements to the Voria2 Ingest API.'));

		s = m.section(form.NamedSection, 'general', 'davisweather', _('General Settings'));
		s.addremove = false;

		o = s.option(form.Flag, 'enabled', _('Enabled'),
			_('Run the daemon at boot. Save &amp; Apply starts or stops it.'));
		o.rmempty = false;
		o.default = '0';

		o = s.option(form.Value, 'port', _('Serial port'),
			_('WeatherLink data logger device, e.g. /dev/ttyUSB0.'));
		o.placeholder = '/dev/ttyUSB0';
		o.rmempty = false;
		o.datatype = 'device';

		o = s.option(form.Value, 'baud', _('Baud rate'),
			_('Davis consoles default to 19200.'));
		o.placeholder = '19200';
		o.default = '19200';
		o.datatype = 'uinteger';

		o = s.option(form.Value, 'interval', _('Poll interval'),
			_('Go duration, e.g. 30s, 1m, 2m, 5m.'));
		o.placeholder = '2m';
		o.default = '2m';
		o.rmempty = false;

		o = s.option(form.ListValue, 'bucket', _('Rain bucket size'),
			_('Tipping-bucket gauge installed on the station.'));
		o.value('0.2mm', _('0.2 mm (metric)'));
		o.value('0.01in', _('0.01 in (imperial / US)'));
		o.default = '0.2mm';
		o.rmempty = false;

		o = s.option(form.ListValue, 'buffering', _('Offline buffering'),
			_('Stateless drops failed uploads and retries with fresh data; SQLite buffers them on flash (beware wear and space on small routers).'));
		o.value('none', _('Stateless (no database)'));
		o.value('sqlite', _('SQLite buffer'));
		o.default = 'none';
		o.rmempty = false;

		o = s.option(form.Value, 'db_path', _('SQLite database path'),
			_('Only used with SQLite buffering, e.g. on extroot.'));
		o.placeholder = '/etc/davisweather/weather.db';
		o.default = '/etc/davisweather/weather.db';
		o.depends = { buffering: 'sqlite' };

		s = m.section(form.NamedSection, 'voria2', 'davisweather', _('Voria2 API'));
		s.addremove = false;

		o = s.option(form.Value, 'api_url', _('API base URL'),
			_('Voria2 API base URL.'));
		o.placeholder = 'https://api.voria2.io';
		o.default = 'https://api.voria2.io';
		o.rmempty = false;

		o = s.option(form.Value, 'api_key', _('API key'),
			_('Station API key starting with vsk_ (stored unencrypted in UCI).'));
		o.password = true;
		o.rmempty = false;

		return m.render().then(function(mapNode) {
			return E('div', {}, [
				E('p', { 'class': 'cbi-section-descr' }, statusText),
				mapNode
			]);
		});
	}
});
