// ProxS3 - S3 Storage Plugin UI for Proxmox VE

Ext.define('PVE.storage.S3InputPanel', {
    extend: 'PVE.panel.StorageBase',

    initComponent: function() {
	var me = this;

	me.column1 = [
	    {
		xtype: me.isCreate ? 'textfield' : 'displayfield',
		name: 'endpoint',
		fieldLabel: 'Endpoint',
		allowBlank: false,
		emptyText: 'hostname only, e.g. s3.us-east-1.amazonaws.com',
	    },
	    {
		xtype: me.isCreate ? 'textfield' : 'displayfield',
		name: 'bucket',
		fieldLabel: 'Bucket',
		allowBlank: false,
		emptyText: 'my-bucket-name',
	    },
	    {
		xtype: 'textfield',
		name: 'region',
		fieldLabel: 'Region',
		emptyText: 'us-east-1',
		allowBlank: true,
	    },
	    {
		xtype: 'pveContentTypeSelector',
		name: 'content',
		multiSelect: true,
		fieldLabel: gettext('Content'),
		allowBlank: false,
		cts: ['images', 'iso', 'vztmpl', 'snippets', 'backup', 'import'],
	    },
	];

	me.column2 = [
	    {
		xtype: 'textfield',
		name: 'access-key',
		fieldLabel: 'Access Key',
		allowBlank: true,
		emptyText: 'optional for public buckets',
	    },
	    {
		xtype: 'textfield',
		name: 'secret-key',
		fieldLabel: 'Secret Key',
		inputType: 'password',
		allowBlank: true,
		emptyText: 'optional for public buckets',
	    },
	    {
		xtype: 'proxmoxcheckbox',
		name: 'use-ssl',
		fieldLabel: 'Use SSL',
		checked: true,
		uncheckedValue: 0,
	    },
	    {
		xtype: 'proxmoxKVComboBox',
		name: 'path-style',
		fieldLabel: 'URL Style',
		comboItems: [
		    ['0', 'Virtual-hosted (bucket.endpoint)'],
		    ['1', 'Path (endpoint/bucket)'],
		],
		value: '0',
		allowBlank: false,
	    },
	    {
		xtype: 'proxmoxintegerfield',
		name: 'cache-max-age',
		fieldLabel: 'Cache Max Age',
		emptyText: '0 (keep forever)',
		minValue: 0,
		allowBlank: true,
		deleteEmpty: !me.isCreate,
	    },
	    {
		xtype: 'proxmoxintegerfield',
		name: 'part-size-mb',
		fieldLabel: 'Part Size (MB)',
		emptyText: '64 (default)',
		minValue: 5,
		allowBlank: true,
		deleteEmpty: !me.isCreate,
	    },
	];

	me.callParent();
    },
});

// Register the S3 type in the storage schema so it appears in the Add dropdown
if (typeof PVE !== 'undefined' && PVE.Utils && PVE.Utils.storageSchema) {
    PVE.Utils.storageSchema.s3 = {
	name: 'S3',
	ipanel: 'S3InputPanel',
	faIcon: 'cloud',
	backups: true,
    };
}
