<?php
namespace OCA\ShareBridge\Controller;

use OCP\AppFramework\Controller;
use OCP\AppFramework\Http;
use OCP\AppFramework\Http\JSONResponse;
use OCP\Files\File;
use OCP\Files\Folder;
use OCP\IRequest;
use OCP\Share\IManager as IShareManager;

/**
 * Returns the SHA1 checksum for a file in a public share.
 *
 * Nextcloud's FilesPlugin only registers oc:checksums for authenticated WebDAV
 * sessions ($user !== null), so public share PROPFIND responses never include
 * checksums. This endpoint runs server-side with full NC privileges and looks
 * up the stored checksum directly, using the share token as the credential.
 */
class ShareChecksumController extends Controller {
    public function __construct(
        string $appName,
        IRequest $request,
        private IShareManager $shareManager,
    ) {
        parent::__construct($appName, $request);
    }

    /**
     * @NoAdminRequired
     * @NoCSRFRequired
     * @PublicPage
     */
    public function get(string $token, string $path = ''): JSONResponse {
        try {
            $share = $this->shareManager->getShareByToken($token);
        } catch (\Exception $e) {
            return new JSONResponse(['error' => 'share not found'], Http::STATUS_NOT_FOUND);
        }

        $node = $share->getNode();

        // For folder shares, navigate to the requested file within the share.
        // For single-file shares, $node is already the File — ignore $path.
        if ($node instanceof Folder && $path !== '') {
            try {
                $node = $node->get($path);
            } catch (\Exception $e) {
                return new JSONResponse(['error' => 'file not found'], Http::STATUS_NOT_FOUND);
            }
        }

        if (!($node instanceof File)) {
            return new JSONResponse(['error' => 'not a file'], Http::STATUS_BAD_REQUEST);
        }

        // getChecksum() returns "SHA1:<hex> MD5:<hex> ADLER32:<hex>" or "".
        $sha1 = '';
        foreach (explode(' ', $node->getChecksum()) as $part) {
            if (str_starts_with($part, 'SHA1:')) {
                $sha1 = strtolower(substr($part, 5));
                break;
            }
        }

        return new JSONResponse(['sha1' => $sha1]);
    }
}
