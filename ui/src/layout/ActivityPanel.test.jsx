import React from 'react'
import { render, screen, fireEvent } from '@testing-library/react'
import { Provider } from 'react-redux'
import { createStore, combineReducers } from 'redux'
import { describe, it, beforeEach } from 'vitest'

import ActivityPanel from './ActivityPanel'
import { activityReducer } from '../reducers'
import config from '../config'
import subsonic from '../subsonic'

vi.mock('../subsonic', () => ({
  default: {
    getScanStatus: vi.fn(() =>
      Promise.resolve({
        json: {
          'subsonic-response': {
            status: 'ok',
            scanStatus: { error: 'Scan failed' },
          },
        },
      }),
    ),
    startScan: vi.fn(),
  },
}))

describe('<ActivityPanel />', () => {
  let store

  beforeEach(() => {
    store = createStore(combineReducers({ activity: activityReducer }), {
      activity: {
        scanStatus: {
          scanning: false,
          folderCount: 0,
          count: 0,
          error: 'Scan failed',
          elapsedTime: 0,
          r128Analyzing: false,
          r128Completed: 0,
          r128Total: 0,
        },
        serverStart: { version: config.version, startTime: Date.now() },
      },
    })
  })

  it('clears the error icon after opening the panel', () => {
    render(
      <Provider store={store}>
        <ActivityPanel />
      </Provider>,
    )

    const button = screen.getByRole('button')
    expect(screen.getByTestId('activity-error-icon')).toBeInTheDocument()

    fireEvent.click(button)

    expect(screen.getByTestId('activity-ok-icon')).toBeInTheDocument()
    expect(screen.getByText('Scan failed')).toBeInTheDocument()
  })

  it('shows R128 analysis progress when analyzing', () => {
    const storeWithR128 = createStore(combineReducers({ activity: activityReducer }), {
      activity: {
        scanStatus: {
          scanning: true,
          folderCount: 5,
          count: 100,
          error: '',
          elapsedTime: 0,
          r128Analyzing: true,
          r128Completed: 3,
          r128Total: 10,
        },
        serverStart: { version: config.version, startTime: Date.now() },
      },
    })

    render(
      <Provider store={storeWithR128}>
        <ActivityPanel />
      </Provider>,
    )

    const button = screen.getByRole('button')
    fireEvent.click(button)

    expect(screen.getByText('正在分析:')).toBeInTheDocument()
    expect(screen.getByText('3/10')).toBeInTheDocument()
  })

  it('hides R128 analysis progress when not analyzing', () => {
    render(
      <Provider store={store}>
        <ActivityPanel />
      </Provider>,
    )

    const button = screen.getByRole('button')
    fireEvent.click(button)

    expect(screen.queryByText('正在分析:')).not.toBeInTheDocument()
  })

  it('shows correct R128 progress at different stages', () => {
    // Test case: 0/10 (just started)
    const storeStarted = createStore(combineReducers({ activity: activityReducer }), {
      activity: {
        scanStatus: {
          scanning: true,
          folderCount: 5,
          count: 100,
          error: '',
          elapsedTime: 0,
          r128Analyzing: true,
          r128Completed: 0,
          r128Total: 10,
        },
        serverStart: { version: config.version, startTime: Date.now() },
      },
    })

    const { rerender } = render(
      <Provider store={storeStarted}>
        <ActivityPanel />
      </Provider>,
    )

    let button = screen.getByRole('button')
    fireEvent.click(button)

    expect(screen.getByText('正在分析:')).toBeInTheDocument()
    expect(screen.getByText('0/10')).toBeInTheDocument()

    // Test case: 10/10 (completed, should still show until analyzing becomes false)
    const storeCompleted = createStore(combineReducers({ activity: activityReducer }), {
      activity: {
        scanStatus: {
          scanning: true,
          folderCount: 5,
          count: 100,
          error: '',
          elapsedTime: 0,
          r128Analyzing: true,
          r128Completed: 10,
          r128Total: 10,
        },
        serverStart: { version: config.version, startTime: Date.now() },
      },
    })

    rerender(
      <Provider store={storeCompleted}>
        <ActivityPanel />
      </Provider>,
    )

    button = screen.getByRole('button')
    fireEvent.click(button)

    expect(screen.getByText('正在分析:')).toBeInTheDocument()
    expect(screen.getByText('10/10')).toBeInTheDocument()
  })
})
