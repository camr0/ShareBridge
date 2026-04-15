<?php
namespace OCA\ShareBridge\Tests\Controller;

use OCA\ShareBridge\Controller\SettingsController;
use OCP\AppFramework\Http\JSONResponse;
use OCP\IConfig;
use OCP\IRequest;
use PHPUnit\Framework\MockObject\MockObject;
use PHPUnit\Framework\TestCase;

class SettingsControllerTest extends TestCase {
    private IConfig&MockObject $config;
    private SettingsController $controller;

    protected function setUp(): void {
        $this->config = $this->createMock(IConfig::class);
        $this->controller = new SettingsController(
            'sharebridge',
            $this->createMock(IRequest::class),
            $this->config,
            'testuser'
        );
    }

    public function testGetReturnsEmptyStringsForNewUser(): void {
        $this->config->method('getUserValue')
            ->willReturnMap([
                ['testuser', 'sharebridge', 'agent_url', '', ''],
                ['testuser', 'sharebridge', 'api_key',   '', ''],
            ]);

        $response = $this->controller->get();

        $this->assertInstanceOf(JSONResponse::class, $response);
        $this->assertEquals(['agent_url' => '', 'api_key' => ''], $response->getData());
    }

    public function testGetReturnsStoredValues(): void {
        $this->config->method('getUserValue')
            ->willReturnMap([
                ['testuser', 'sharebridge', 'agent_url', '', 'http://localhost:7878'],
                ['testuser', 'sharebridge', 'api_key',   '', 'sb_agent_abc123'],
            ]);

        $response = $this->controller->get();

        $this->assertEquals([
            'agent_url' => 'http://localhost:7878',
            'api_key'   => 'sb_agent_abc123',
        ], $response->getData());
    }

    public function testUpdateSavesValues(): void {
        $this->config->expects($this->exactly(2))
            ->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value): void {
                // just verify it's called — no return needed
            });

        $response = $this->controller->update('http://localhost:7878', 'sb_agent_abc123');

        $this->assertEquals(['status' => 'ok'], $response->getData());
    }

    public function testUpdateSavesCorrectKeys(): void {
        $calls = [];
        $this->config->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value) use (&$calls): void {
                $calls[] = [$uid, $app, $key, $value];
            });

        $this->controller->update('http://localhost:7878', 'sb_agent_abc123');

        $this->assertContains(['testuser', 'sharebridge', 'agent_url', 'http://localhost:7878'], $calls);
        $this->assertContains(['testuser', 'sharebridge', 'api_key',   'sb_agent_abc123'], $calls);
    }

    public function testSettingsArePerUser(): void {
        $otherController = new SettingsController(
            'sharebridge',
            $this->createMock(IRequest::class),
            $this->config,
            'otheruser'
        );

        // testuser has a value, otheruser has empty
        $this->config->method('getUserValue')
            ->willReturnMap([
                ['testuser',  'sharebridge', 'agent_url', '', 'http://localhost:7878'],
                ['testuser',  'sharebridge', 'api_key',   '', 'sb_key1'],
                ['otheruser', 'sharebridge', 'agent_url', '', ''],
                ['otheruser', 'sharebridge', 'api_key',   '', ''],
            ]);

        $testUserResponse  = $this->controller->get();
        $otherUserResponse = $otherController->get();

        $this->assertEquals('http://localhost:7878', $testUserResponse->getData()['agent_url']);
        $this->assertEquals('', $otherUserResponse->getData()['agent_url']);
    }

    public function testSaveNcShareIdStoresMapping(): void {
        $this->config->method('getUserValue')
            ->with('testuser', 'sharebridge', 'nc_share_ids', '{}')
            ->willReturn('{}');

        $saved = null;
        $this->config->expects($this->once())
            ->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value) use (&$saved): void {
                $saved = json_decode($value, true);
            });

        $this->controller->saveNcShareId('ABC123', '42');

        $this->assertEquals(['ABC123' => '42'], $saved);
    }

    public function testSaveNcShareIdPreservesExistingMappings(): void {
        $this->config->method('getUserValue')
            ->willReturn(json_encode(['XYZ789' => '10']));

        $saved = null;
        $this->config->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value) use (&$saved): void {
                $saved = json_decode($value, true);
            });

        $this->controller->saveNcShareId('ABC123', '42');

        $this->assertEquals(['XYZ789' => '10', 'ABC123' => '42'], $saved);
    }

    public function testGetNcShareIdReturnsStoredId(): void {
        $this->config->method('getUserValue')
            ->willReturn(json_encode(['ABC123' => '42']));

        $response = $this->controller->getNcShareId('ABC123');

        $this->assertEquals(200, $response->getStatus());
        $this->assertEquals(['nc_share_id' => '42'], $response->getData());
    }

    public function testGetNcShareIdReturns404ForUnknownCode(): void {
        $this->config->method('getUserValue')
            ->willReturn('{}');

        $response = $this->controller->getNcShareId('UNKNOWN');

        $this->assertEquals(404, $response->getStatus());
    }

    public function testDeleteNcShareIdRemovesEntry(): void {
        $this->config->method('getUserValue')
            ->willReturn(json_encode(['ABC123' => '42', 'XYZ789' => '10']));

        $saved = null;
        $this->config->expects($this->once())
            ->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value) use (&$saved): void {
                $saved = json_decode($value, true);
            });

        $this->controller->deleteNcShareId('ABC123');

        $this->assertArrayNotHasKey('ABC123', $saved);
        $this->assertArrayHasKey('XYZ789', $saved);
    }

    public function testDeleteNcShareIdOnUnknownCodeIsNoop(): void {
        $this->config->method('getUserValue')
            ->willReturn(json_encode(['XYZ789' => '10']));

        $this->config->expects($this->once())->method('setUserValue');

        $response = $this->controller->deleteNcShareId('UNKNOWN');

        $this->assertEquals(200, $response->getStatus());
    }
}