<?php
namespace OCA\ShareBridge\Controller;

use OCP\AppFramework\Controller;
use OCP\AppFramework\Http\JSONResponse;
use OCP\IConfig;
use OCP\IRequest;

/**
 * Proxies requests from the browser to the ShareBridge agent.
 *
 * Two reasons for this proxy:
 *  1. NC's CSP (connect-src 'self') blocks direct browser→agent requests.
 *  2. Mixed content: NC is served over HTTPS but the agent may be HTTP.
 *
 * We use PHP's curl directly (not IClientService) to avoid NC's SSRF validator,
 * which would block private/LAN IP addresses where the agent typically runs.
 * The agent URL is user-configured and protected by NC authentication, so the
 * SSRF risk is minimal — a user can only proxy to their own configured agent.
 */
class AgentProxyController extends Controller {
    public function __construct(
        string $appName,
        IRequest $request,
        private IConfig $config,
        private string $userId,
    ) {
        parent::__construct($appName, $request);
    }

    private function agentUrl(): string {
        return rtrim($this->config->getUserValue($this->userId, 'sharebridge', 'agent_url', ''), '/');
    }

    private function apiKey(): string {
        return $this->config->getUserValue($this->userId, 'sharebridge', 'api_key', '');
    }

    private function curlGet(string $url): JSONResponse {
        $ch = curl_init($url);
        curl_setopt_array($ch, [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_HTTPHEADER     => ['X-API-Key: ' . $this->apiKey()],
            CURLOPT_TIMEOUT        => 10,
        ]);
        $body = curl_exec($ch);
        $code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
        $err  = curl_error($ch);
        curl_close($ch);

        if ($body === false) {
            return new JSONResponse(['error' => $err ?: 'curl failed'], 502);
        }
        $data = json_decode($body, true);
        return new JSONResponse($data ?? [], $code >= 200 && $code < 300 ? 200 : $code);
    }

    private function curlPost(string $url, array $params): JSONResponse {
        $ch = curl_init($url);
        curl_setopt_array($ch, [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_POST           => true,
            CURLOPT_POSTFIELDS     => json_encode($params),
            CURLOPT_HTTPHEADER     => [
                'X-API-Key: ' . $this->apiKey(),
                'Content-Type: application/json',
            ],
            CURLOPT_TIMEOUT        => 10,
        ]);
        $body = curl_exec($ch);
        $code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
        $err  = curl_error($ch);
        curl_close($ch);

        if ($body === false) {
            return new JSONResponse(['error' => $err ?: 'curl failed'], 502);
        }
        $data = json_decode($body, true);
        return new JSONResponse($data ?? [], $code >= 200 && $code < 300 ? 200 : $code);
    }

    private function curlDelete(string $url): JSONResponse {
        $ch = curl_init($url);
        curl_setopt_array($ch, [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_CUSTOMREQUEST  => 'DELETE',
            CURLOPT_HTTPHEADER     => ['X-API-Key: ' . $this->apiKey()],
            CURLOPT_TIMEOUT        => 10,
        ]);
        $body = curl_exec($ch);
        $code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
        $err  = curl_error($ch);
        curl_close($ch);

        if ($body === false) {
            return new JSONResponse(['error' => $err ?: 'curl failed'], 502);
        }
        return new JSONResponse([], $code >= 200 && $code < 300 ? 200 : $code);
    }

    /** @NoAdminRequired */
    public function listShares(string $file_id = ''): JSONResponse {
        $url = $this->agentUrl() . '/api/v1/shares';
        if ($file_id !== '') {
            $url .= '?file_id=' . urlencode($file_id);
        }
        return $this->curlGet($url);
    }

    /** @NoAdminRequired */
    public function createShare(): JSONResponse {
        return $this->curlPost(
            $this->agentUrl() . '/api/v1/shares',
            $this->request->getParams()
        );
    }

    /** @NoAdminRequired */
    public function revokeShare(string $code): JSONResponse {
        return $this->curlDelete(
            $this->agentUrl() . '/api/v1/shares/' . urlencode($code)
        );
    }

    /** @NoAdminRequired */
    public function agentSettings(): JSONResponse {
        return $this->curlGet($this->agentUrl() . '/api/v1/settings');
    }
}
