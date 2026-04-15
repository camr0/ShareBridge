<?php
namespace OCA\ShareBridge\Controller;

use OCP\AppFramework\Controller;
use OCP\AppFramework\Http\JSONResponse;
use OCP\IConfig;
use OCP\IRequest;

class SettingsController extends Controller {
    public function __construct(
        string $appName,
        IRequest $request,
        private IConfig $config,
        private string $userId,
    ) {
        parent::__construct($appName, $request);
    }

    public function get(): JSONResponse {
        return new JSONResponse([
            'agent_url' => $this->config->getUserValue($this->userId, 'sharebridge', 'agent_url', ''),
            'api_key'   => $this->config->getUserValue($this->userId, 'sharebridge', 'api_key',   ''),
        ]);
    }

    public function update(string $agent_url, string $api_key): JSONResponse {
        $this->config->setUserValue($this->userId, 'sharebridge', 'agent_url', $agent_url);
        $this->config->setUserValue($this->userId, 'sharebridge', 'api_key',   $api_key);
        return new JSONResponse(['status' => 'ok']);
    }

    public function saveNcShareId(string $code, string $nc_share_id): JSONResponse {
        $blob    = $this->config->getUserValue($this->userId, 'sharebridge', 'nc_share_ids', '{}');
        $mapping = json_decode($blob, true) ?: [];
        $mapping[$code] = $nc_share_id;
        $this->config->setUserValue($this->userId, 'sharebridge', 'nc_share_ids', json_encode($mapping));
        return new JSONResponse(['status' => 'ok']);
    }

    public function getNcShareId(string $code): JSONResponse {
        $blob    = $this->config->getUserValue($this->userId, 'sharebridge', 'nc_share_ids', '{}');
        $mapping = json_decode($blob, true) ?: [];
        if (!array_key_exists($code, $mapping)) {
            return new JSONResponse(['message' => 'not found'], 404);
        }
        return new JSONResponse(['nc_share_id' => $mapping[$code]]);
    }

    public function deleteNcShareId(string $code): JSONResponse {
        $blob    = $this->config->getUserValue($this->userId, 'sharebridge', 'nc_share_ids', '{}');
        $mapping = json_decode($blob, true) ?: [];
        unset($mapping[$code]);
        $this->config->setUserValue($this->userId, 'sharebridge', 'nc_share_ids', json_encode($mapping));
        return new JSONResponse(['status' => 'ok']);
    }
}