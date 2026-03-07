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
	    },
	    {
		xtype: me.isCreate ? 'textfield' : 'displayfield',
		name: 'bucket',
		fieldLabel: 'Bucket',
		allowBlank: false,
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
		value: 'iso',
		multiSelect: true,
		fieldLabel: gettext('Content'),
		allowBlank: false,
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
		xtype: 'proxmoxcheckbox',
		name: 'path-style',
		fieldLabel: 'Path Style',
		checked: false,
		uncheckedValue: 0,
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
