# frozen_string_literal: true

set :build_domain, 'market-s2.bitzlato.com'
set :stage, 's2'

server ENV.fetch( 'STAGING_SERVER_2' ), user: fetch(:user), roles: fetch(:roles)
