<?php
namespace OCA\ShareBridge\Settings;

use OCP\AppFramework\Http\TemplateResponse;
use OCP\Settings\ISettings;

class PersonalSection implements ISettings {
    public function getForm(): TemplateResponse {
        return new TemplateResponse('sharebridge', 'personal_settings', [], 'blank');
    }

    public function getSection(): string {
        return 'personal-info';
    }

    public function getPriority(): int {
        return 50;
    }
}