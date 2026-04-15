<?php
return [
    'routes' => [
        ['name' => 'settings#get',          'url' => '/api/settings',                    'verb' => 'GET'],
        ['name' => 'settings#update',       'url' => '/api/settings',                    'verb' => 'PUT'],
        ['name' => 'settings#saveNcShareId',   'url' => '/api/shares/{code}/nc-share-id',   'verb' => 'PUT'],
        ['name' => 'settings#getNcShareId',    'url' => '/api/shares/{code}/nc-share-id',   'verb' => 'GET'],
        ['name' => 'settings#deleteNcShareId', 'url' => '/api/shares/{code}/nc-share-id',   'verb' => 'DELETE'],
    ],
];