import { ClockIcon, type ServicePlugin } from '@holistic/ui';
import { Dashboard } from './Dashboard';

// This service's dashboard plugin. Linked into holistic/frontend/external/<id> at install
// time and discovered by the host SPA's build-time registry. `id` MUST equal the link dir
// name and the permissions manifest's "service" field.
const plugin: ServicePlugin = {
  id: 'icaly',
  displayName: 'Calendar',
  icon: ClockIcon,
  order: 50,
  Component: Dashboard,
};

export default plugin;
