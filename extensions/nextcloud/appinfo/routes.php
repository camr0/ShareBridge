<?php
return [
    'routes' => [
        // Settings CRUD
        ['name' => 'settings#get',             'url' => '/api/settings',                  'verb' => 'GET'],
        ['name' => 'settings#update',          'url' => '/api/settings',                  'verb' => 'PUT'],
        ['name' => 'settings#saveNcShareId',   'url' => '/api/shares/{code}/nc-share-id', 'verb' => 'PUT'],
        ['name' => 'settings#getNcShareId',    'url' => '/api/shares/{code}/nc-share-id', 'verb' => 'GET'],
        ['name' => 'settings#deleteNcShareId', 'url' => '/api/shares/{code}/nc-share-id', 'verb' => 'DELETE'],

        // Public checksum lookup — no NC auth required, share token is the credential
        ['name' => 'share_checksum#get', 'url' => '/api/public/share-checksum', 'verb' => 'GET'],

        // Agent proxy — bypasses CSP and mixed-content restrictions
        ['name' => 'agent_proxy#list_shares',   'url' => '/api/agent/shares',        'verb' => 'GET'],
        ['name' => 'agent_proxy#create_share',  'url' => '/api/agent/shares',        'verb' => 'POST'],
        ['name' => 'agent_proxy#revoke_share',  'url' => '/api/agent/shares/{code}', 'verb' => 'DELETE'],
        ['name' => 'agent_proxy#agent_settings','url' => '/api/agent/settings',      'verb' => 'GET'],
    ],
];
