<?php
namespace OCA\ShareBridge\AppInfo;

use OCA\ShareBridge\Settings\PersonalSection;
use OCP\AppFramework\App;
use OCP\AppFramework\Bootstrap\IBootContext;
use OCP\AppFramework\Bootstrap\IBootstrap;
use OCP\AppFramework\Bootstrap\IRegistrationContext;
use OCP\Util;

class Application extends App implements IBootstrap {
    public const APP_ID = 'sharebridge';

    public function __construct() {
        parent::__construct(self::APP_ID);
    }

    public function register(IRegistrationContext $context): void {
        $context->registerSetting(PersonalSection::class);
    }

    public function boot(IBootContext $context): void {
        Util::addScript(self::APP_ID, 'sharebridge-main');
    }
}