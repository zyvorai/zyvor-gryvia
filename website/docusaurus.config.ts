import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Gryvia',
  tagline: 'Open GPU orchestration for Kubernetes.',
  favicon: 'img/favicon.svg',

  future: {
    v4: true,
  },

  url: 'https://zyvorai.github.io',
  baseUrl: '/zyvor-gryvia/',

  organizationName: 'zyvorai',
  projectName: 'zyvor-gryvia',

  onBrokenLinks: 'throw',

  markdown: {
    // Existing docs are plain CommonMark; don't parse them as MDX.
    format: 'detect',
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  // Screenshots live in docs/ux and are served in place.
  staticDirectories: ['static', '../docs/ux'],

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/zyvorai/zyvor-gryvia/tree/main/website/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'img/gryvia-share-card.png',
    colorMode: {
      defaultMode: 'dark',
      respectPrefersColorScheme: false,
    },
    navbar: {
      title: 'Gryvia',
      logo: {alt: 'Gryvia', src: 'img/favicon.svg'},
      items: [
        {type: 'docSidebar', sidebarId: 'docsSidebar', position: 'left', label: 'Docs'},
        {href: 'https://github.com/zyvorai/zyvor-gryvia', label: 'GitHub', position: 'right'},
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {label: 'Quickstart', to: '/docs/getting-started/quickstart'},
            {label: 'CLI guide', to: '/docs/guides/CLI_GUIDE'},
            {label: 'Roadmap', to: '/docs/guides/ROADMAP'},
          ],
        },
        {
          title: 'Project',
          items: [
            {label: 'GitHub', href: 'https://github.com/zyvorai/zyvor-gryvia'},
            {label: 'Issues', href: 'https://github.com/zyvorai/zyvor-gryvia/issues'},
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} Gryvia contributors.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['bash', 'yaml', 'go', 'rust', 'python', 'json'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
