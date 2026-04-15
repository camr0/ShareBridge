const path = require('path')
const webpackConfig = require('@nextcloud/webpack-vue-config')

// Remove the default TS rule so we can replace it with our configured version
const rulesWithoutTS = webpackConfig.module.rules.filter(rule => !rule.test?.test?.('.ts'))

module.exports = {
    ...webpackConfig,
    entry: {
        'sharebridge-main': path.join(__dirname, 'src', 'main.ts'),
    },
    module: {
        rules: [
            ...rulesWithoutTS,
            {
                test: /\.tsx?$/,
                use: [
                    'babel-loader',
                    {
                        loader: 'ts-loader',
                        options: {
                            appendTsSuffixTo: [/\.vue$/],
                            transpileOnly: true,
                        },
                    },
                ],
                exclude: /node_modules/,
            },
        ],
    },
}